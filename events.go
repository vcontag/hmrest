package main

// Live-Events: Das Add-on meldet sich per init() bei den CCU-Schnittstellen an
// und empfängt Wertänderungen über einen lokalen XML-RPC-Callback-Server.
// Diese werden per Server-Sent Events (/api/events) weitergereicht.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Interface string      `json:"interface"`
	Address   string      `json:"address"`
	Param     string      `json:"param"`
	Value     interface{} `json:"value"`
	TS        time.Time   `json:"ts"`
}

type Hub struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	last   map[string]Event
	closed bool
}

func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}, last: map[string]Event{}}
}

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 256)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.subs[ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[e.Address+"|"+e.Param] = e
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // langsamer Client: Event verwerfen
		}
	}
}

func (h *Hub) Last() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, 0, len(h.last))
	for _, e := range h.last {
		out = append(out, e)
	}
	return out
}

func (h *Hub) Count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.subs) }

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}

// ---------- Callback-Server ----------

type Callback struct {
	ccu    *CCU
	hub    *Hub
	listen string
	url    string
	prefix string
	every  time.Duration
	// BIN-RPC-Callback für Schnittstellen wie CUxD
	binListen string
	binURL    string

	mu       sync.Mutex
	lastInit map[string]time.Time
	lastEvt  map[string]time.Time

	// Events werden entkoppelt verarbeitet: die CCU bekommt sofort Antwort,
	// ein Worker prüft/wandelt die Werte und veröffentlicht sie in Reihenfolge.
	queue chan Event
}

func NewCallback(ccu *CCU, hub *Hub, listen, url string, every time.Duration) *Callback {
	cb := &Callback{ccu: ccu, hub: hub, listen: listen, url: url, prefix: "hmrest-", every: every,
		lastInit: map[string]time.Time{}, lastEvt: map[string]time.Time{}, queue: make(chan Event, 4096)}
	go cb.worker()
	return cb
}

func (cb *Callback) worker() {
	for e := range cb.queue {
		v, ok := cb.ccu.NormalizeEvent(e.Address, e.Param, e.Value)
		if !ok {
			continue // unbrauchbarer Wert (z.B. "" für eine Zahl) – nicht weitergeben
		}
		e.Value = v
		cb.hub.Publish(e)
	}
}

// SetBin aktiviert den BIN-RPC-Callback (listen z.B. 127.0.0.1:9127, url xmlrpc_bin://127.0.0.1:9127).
func (cb *Callback) SetBin(listen, url string) { cb.binListen, cb.binURL = listen, url }

func (cb *Callback) urlFor(i *Iface) string {
	if i.IsBin() && cb.binURL != "" {
		return cb.binURL
	}
	return cb.url
}

func (cb *Callback) ListenAndServe() {
	// immer starten: das Protokoll kann auch erst zur Laufzeit erkannt werden
	if cb.binListen != "" {
		go func() {
			if err := ServeBinRPC(cb.binListen, cb.dispatch); err != nil {
				log.Printf("BIN-RPC-Callback-Server: %v", err)
			}
		}()
	}
	srv := &http.Server{Addr: cb.listen, Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("Callback-Server: %v", err)
	}
}

func (cb *Callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, params, err := parseRequest(r.Body)
	if err != nil {
		log.Printf("Callback: fehlerhafte Anfrage: %v", err)
	}
	res := cb.dispatch(method, params)
	w.Header().Set("Content-Type", "text/xml")
	_ = writeResponse(w, res)
}

func (cb *Callback) dispatch(method string, params []interface{}) interface{} {
	switch method {
	case "system.multicall":
		out := []interface{}{}
		if len(params) > 0 {
			calls, _ := params[0].([]interface{})
			for _, c := range calls {
				m, _ := c.(map[string]interface{})
				name, _ := m["methodName"].(string)
				p, _ := m["params"].([]interface{})
				out = append(out, []interface{}{cb.dispatch(name, p)})
			}
		}
		return out
	case "event":
		if len(params) >= 4 {
			iface := strings.TrimPrefix(fmt.Sprint(params[0]), cb.prefix)
			cb.mu.Lock()
			cb.lastEvt[iface] = time.Now()
			cb.mu.Unlock()
			e := Event{Interface: iface, Address: fmt.Sprint(params[1]), Param: fmt.Sprint(params[2]), Value: params[3], TS: time.Now()}
			select {
			case cb.queue <- e:
			default:
				log.Printf("Event-Warteschlange voll – Event %s %s verworfen", e.Address, e.Param)
			}
		}
		return ""
	case "listDevices":
		return []interface{}{}
	case "newDevices", "deleteDevices", "updateDevice", "replaceDevice", "readdedDevice":
		cb.ccu.InvalidateIndex()
		return ""
	case "system.listMethods":
		return []interface{}{"system.multicall", "system.listMethods", "event", "listDevices", "newDevices",
			"deleteDevices", "updateDevice", "replaceDevice", "readdedDevice"}
	}
	return ""
}

// Run meldet den Callback regelmäßig bei allen Schnittstellen an.
func (cb *Callback) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		for _, i := range cb.ccu.Interfaces() {
			cb.mu.Lock()
			last := cb.lastInit[i.Name]
			cb.mu.Unlock()
			if !last.IsZero() && time.Since(last) < cb.every {
				continue
			}
			if _, err := i.Call("init", cb.urlFor(i), cb.prefix+i.Name); err != nil {
				if !last.IsZero() {
					log.Printf("init %s fehlgeschlagen: %v", i.Name, err)
				}
				cb.mu.Lock()
				delete(cb.lastInit, i.Name)
				cb.mu.Unlock()
				continue
			}
			if last.IsZero() {
				log.Printf("Events von %s abonniert (%s)", i.Name, cb.urlFor(i))
			}
			cb.mu.Lock()
			cb.lastInit[i.Name] = time.Now()
			cb.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (cb *Callback) Deregister() {
	for _, i := range cb.ccu.Interfaces() {
		cb.mu.Lock()
		ok := !cb.lastInit[i.Name].IsZero()
		cb.mu.Unlock()
		if ok {
			_, _ = i.Call("init", cb.urlFor(i))
		}
	}
}

func (cb *Callback) Status() map[string]interface{} {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st := map[string]interface{}{}
	for _, i := range cb.ccu.Interfaces() {
		s := map[string]interface{}{"subscribed": !cb.lastInit[i.Name].IsZero()}
		if t, ok := cb.lastEvt[i.Name]; ok {
			s["lastEvent"] = t
		}
		st[i.Name] = s
	}
	return st
}
