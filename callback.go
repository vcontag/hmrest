package main

// Anmeldung bei den CCU-Schnittstellen (init) und Callback-Empfang.
//
// Jedes init() legt im HMIPServer intern einen neuen HTTP-Client an, der erst beim
// Abmelden wieder freigegeben wird. Häufige inits führen dort zu einem
// File-Descriptor-Leck. Daher gilt:
//   - feste Interface-ID je Schnittstelle ("hmrest-<Name>"), auch über Neustarts gleich
//   - init genau einmal beim Start je Schnittstelle
//   - erneutes init nur bei echtem Verbindungsverlust: nach Ruhezeit ohne Event wird
//     ping gesendet (Antwort: PONG-Event); bleibt auch das aus, folgt init – mit
//     Backoff, nie in einer Schleife, je Schnittstelle nur eines gleichzeitig
//   - beim Beenden Abmeldung mit init(<url>, "")

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Wartezeiten bis zum nächsten init-Versuch nach Fehlschlag oder Verbindungsverlust.
// Der Zähler wird erst zurückgesetzt, wenn die Schnittstelle wieder ein Lebenszeichen sendet.
var defaultBackoff = []time.Duration{1 * time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

const idPrefix = "hmrest-"

// interfaceID liefert die feste Interface-ID für init (ohne Zeitstempel, Zufall oder PID).
func interfaceID(name string) string { return idPrefix + name }

type ifState struct {
	lock chan struct{} // je Schnittstelle nur ein init/ping gleichzeitig
	id   string

	registered bool
	url        string // Callback-URL des letzten erfolgreichen init

	initCalls   int // alle init-Aufrufe mit ID (auch fehlgeschlagene)
	deinitCalls int // Abmeldungen mit leerer ID
	pings       int

	lastInit   time.Time
	lastAlive  time.Time // letztes Lebenszeichen (Event, PONG, listDevices, …)
	pingSent   time.Time // ausstehender ping (leer = keiner)
	nextTry    time.Time
	reason     string // Grund des letzten init
	pending    string // Grund für das nächste init
	lastErr    string
	fails      int  // aufeinanderfolgende Fehlschläge/Verluste → Backoff-Stufe
	noPing     bool // Schnittstelle kennt kein ping (z.B. CUxD): nur Erreichbarkeit prüfen
	wasOffline bool // noPing: war nicht erreichbar → bei Rückkehr neu anmelden
	pongSeen   bool // mindestens einmal ein PONG erhalten
	missed     int  // pings ohne PONG in Folge
}

type Callback struct {
	ccu    *CCU
	hub    *Hub
	listen string
	// BIN-RPC-Callback für Schnittstellen wie CUxD
	binListen string

	// Zeiten (in Tests verkürzt)
	idle     time.Duration // Ruhezeit ohne Event, nach der ein ping gesendet wird
	pingWait time.Duration // Wartezeit auf das PONG
	tick     time.Duration
	backoff  []time.Duration

	mu      sync.Mutex
	url     string
	binURL  string
	stopped bool
	states  map[string]*ifState // nach Schnittstellenname
	byID    map[string]string   // Interface-ID → Name

	xmlLn, binLn net.Listener

	// Events werden entkoppelt verarbeitet: die CCU bekommt sofort Antwort,
	// ein Worker prüft/wandelt die Werte und veröffentlicht sie in Reihenfolge.
	queue chan Event
}

// NewCallback: idle ist die Ruhezeit ohne Event, nach der die Verbindung per ping geprüft wird.
func NewCallback(ccu *CCU, hub *Hub, listen, url string, idle time.Duration) *Callback {
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	cb := &Callback{ccu: ccu, hub: hub, listen: listen, url: url, idle: idle,
		pingWait: time.Minute, tick: 15 * time.Second, backoff: defaultBackoff,
		states: map[string]*ifState{}, byID: map[string]string{}, queue: make(chan Event, 4096)}
	for _, i := range ccu.Interfaces() {
		id := interfaceID(i.Name)
		cb.states[i.Name] = &ifState{lock: make(chan struct{}, 1), id: id}
		cb.byID[id] = i.Name
	}
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
func (cb *Callback) SetBin(listen, url string) {
	cb.mu.Lock()
	cb.binListen, cb.binURL = listen, url
	cb.mu.Unlock()
}

// SetURLs übernimmt neue Callback-URLs (z.B. nach dem Neuladen der Konfiguration).
// Nur wenn sich eine URL wirklich ändert, wird die betroffene Schnittstelle ab- und neu angemeldet.
func (cb *Callback) SetURLs(url, binURL string) {
	cb.mu.Lock()
	changed := cb.url != url || cb.binURL != binURL
	cb.url, cb.binURL = url, binURL
	cb.mu.Unlock()
	if changed {
		cb.checkAll()
	}
}

func (cb *Callback) urlFor(i *Iface) string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if i.IsBin() && cb.binURL != "" {
		return cb.binURL
	}
	return cb.url
}

// Listen öffnet die Callback-Ports. Erst danach darf init aufgerufen werden,
// sonst ruft die CCU eine URL auf, unter der niemand antwortet.
func (cb *Callback) Listen() error {
	if cb.listen != "" && cb.xmlLn == nil {
		ln, err := net.Listen("tcp", cb.listen)
		if err != nil {
			return fmt.Errorf("Callback-Server %s: %w", cb.listen, err)
		}
		cb.xmlLn = ln
	}
	cb.mu.Lock()
	binListen := cb.binListen
	cb.mu.Unlock()
	if binListen != "" && cb.binLn == nil {
		ln, err := net.Listen("tcp", binListen)
		if err != nil {
			return fmt.Errorf("BIN-RPC-Callback-Server %s: %w", binListen, err)
		}
		cb.binLn = ln
	}
	return nil
}

// Serve beantwortet Callbacks auf den mit Listen geöffneten Ports (blockiert).
func (cb *Callback) Serve() {
	if cb.binLn != nil {
		go func() {
			if err := serveBinRPC(cb.binLn, cb.dispatch); err != nil {
				log.Printf("BIN-RPC-Callback-Server: %v", err)
			}
		}()
	}
	if cb.xmlLn == nil {
		return
	}
	// Keep-Alive ist erlaubt (die CCU nutzt es), aber mit begrenzter Ruhezeit.
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	if err := srv.Serve(cb.xmlLn); err != nil {
		log.Printf("Callback-Server: %v", err)
	}
}

func (cb *Callback) ListenAndServe() {
	if err := cb.Listen(); err != nil {
		log.Print(err)
	}
	cb.Serve()
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

// alive vermerkt ein Lebenszeichen der Schnittstelle mit dieser Interface-ID.
// Nur echte Events/PONG (delivered=true) setzen den Backoff zurück: listDevices kommt
// nach jedem init und belegt nicht, dass Events ankommen.
func (cb *Callback) alive(id string, delivered, pong bool) string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	name, ok := cb.byID[id]
	if !ok {
		name = strings.TrimPrefix(id, idPrefix)
	}
	if st := cb.states[name]; st != nil {
		st.lastAlive = time.Now()
		if delivered {
			st.pingSent = time.Time{}
			st.missed = 0
			if st.registered {
				st.fails = 0
			}
		}
		if pong {
			st.pongSeen = true
		}
	}
	return name
}

// dispatch beantwortet alle Callbacks sofort (die CCU wartet darauf, Timeouts dort
// führen zu Fehlern) – Events gehen nur in die Warteschlange.
func (cb *Callback) dispatch(method string, params []interface{}) interface{} {
	first := ""
	if len(params) > 0 {
		first = fmt.Sprint(params[0])
	}
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
			addr, param := fmt.Sprint(params[1]), fmt.Sprint(params[2])
			iface := cb.alive(first, true, param == "PONG")
			if param == "PONG" && addr == "CENTRAL" {
				return "" // Antwort auf unseren ping, kein Gerätewert
			}
			e := Event{Interface: iface, Address: addr, Param: param, Value: params[3], TS: time.Now()}
			select {
			case cb.queue <- e:
			default:
				log.Printf("Event-Warteschlange voll – Event %s %s verworfen", e.Address, e.Param)
			}
		}
		return ""
	case "listDevices":
		cb.alive(first, false, false)
		return []interface{}{}
	case "newDevices", "deleteDevices", "updateDevice", "replaceDevice", "readdedDevice":
		cb.alive(first, false, false)
		cb.ccu.InvalidateIndex()
		return ""
	case "system.listMethods":
		return []interface{}{"system.multicall", "system.listMethods", "event", "listDevices", "newDevices",
			"deleteDevices", "updateDevice", "replaceDevice", "readdedDevice"}
	}
	return ""
}

// Run meldet sich einmal bei allen Schnittstellen an und überwacht danach die Verbindung.
func (cb *Callback) Run(ctx context.Context) {
	cb.checkAll()
	t := time.NewTicker(cb.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cb.checkAll()
		}
	}
}

func (cb *Callback) checkAll() {
	for _, i := range cb.ccu.Interfaces() {
		go cb.check(i)
	}
}

func (cb *Callback) backoffFor(n int) time.Duration {
	if len(cb.backoff) == 0 {
		return time.Minute
	}
	if n >= len(cb.backoff) {
		n = len(cb.backoff) - 1
	}
	return cb.backoff[n]
}

// lost markiert die Anmeldung als verloren; das nächste init folgt nach Backoff.
// mu muss gehalten werden.
func (cb *Callback) lostLocked(i *Iface, st *ifState, reason string) {
	wait := cb.backoffFor(st.fails)
	st.fails++
	st.registered = false
	st.pingSent = time.Time{}
	st.pending = reason
	st.nextTry = time.Now().Add(wait)
	log.Printf("Verbindung zu %s verloren (%s) – neue Anmeldung in %s", i.Name, reason, wait)
}

// check prüft eine Schnittstelle. Läuft gerade ein init/ping für sie, wird nichts getan.
func (cb *Callback) check(i *Iface) {
	cb.mu.Lock()
	st := cb.states[i.Name]
	cb.mu.Unlock()
	if st == nil {
		return
	}
	select {
	case st.lock <- struct{}{}:
	default:
		return // nur ein init gleichzeitig je Schnittstelle
	}
	defer func() { <-st.lock }()

	url := cb.urlFor(i)
	now := time.Now()
	cb.mu.Lock()
	if cb.stopped {
		cb.mu.Unlock()
		return
	}
	switch {
	case !st.registered:
		if now.Before(st.nextTry) {
			cb.mu.Unlock()
			return
		}
		reason := st.pending
		if reason == "" {
			reason = "Start"
		}
		cb.mu.Unlock()
		cb.doInit(i, st, url, reason)
		return

	case st.url != url:
		old := st.url
		cb.mu.Unlock()
		cb.doDeinit(i, st, old, "Callback-URL geändert")
		cb.doInit(i, st, url, fmt.Sprintf("Callback-URL geändert (%s → %s)", old, url))
		return

	case !st.pingSent.IsZero():
		// ping ausstehend: das PONG (oder jedes andere Lebenszeichen) setzt pingSent zurück
		if now.Sub(st.pingSent) >= cb.pingWait {
			st.missed++
			if !st.pongSeen && st.missed >= 2 {
				// Schnittstelle beantwortet ping, schickt aber nie ein PONG: nicht endlos neu
				// anmelden, sondern wie bei Schnittstellen ohne ping überwachen.
				st.noPing, st.pingSent, st.lastAlive = true, time.Time{}, now
				log.Printf("%s sendet kein PONG – Verbindung wird über die Erreichbarkeit überwacht", i.Name)
			} else {
				cb.lostLocked(i, st, fmt.Sprintf("kein PONG %s nach ping", cb.pingWait))
			}
		}
		cb.mu.Unlock()
		return
	}
	last := st.lastAlive
	if st.lastInit.After(last) {
		last = st.lastInit
	}
	if now.Sub(last) < cb.idle {
		cb.mu.Unlock()
		return
	}
	noPing, id := st.noPing, st.id
	if !noPing {
		// vor dem Aufruf setzen: das PONG kann schon eintreffen, bevor ping antwortet
		st.pingSent = now
	}
	cb.mu.Unlock()

	if noPing {
		// Ohne ping nur die Erreichbarkeit prüfen; neu anmelden erst nach einem echten Ausfall.
		_, err := i.Call("system.listMethods")
		cb.mu.Lock()
		if err != nil && !isFault(err) {
			if !st.wasOffline {
				log.Printf("%s nicht erreichbar: %v", i.Name, err)
			}
			st.wasOffline = true
		} else if st.wasOffline {
			st.wasOffline = false
			cb.lostLocked(i, st, "Schnittstelle nach Ausfall wieder erreichbar")
			st.nextTry = time.Now() // sofort, die Schnittstelle ist ja wieder da
		} else {
			st.lastAlive = time.Now()
		}
		cb.mu.Unlock()
		return
	}

	_, err := i.Call("ping", id)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch {
	case err == nil:
		st.pings++
	case isFault(err):
		st.pingSent = time.Time{}
		st.noPing = true
		st.lastAlive = time.Now()
		log.Printf("%s kennt kein ping – Verbindung wird über die Erreichbarkeit überwacht", i.Name)
	default:
		cb.lostLocked(i, st, "ping fehlgeschlagen: "+err.Error())
	}
}

func (cb *Callback) doInit(i *Iface, st *ifState, url, reason string) {
	cb.mu.Lock()
	st.initCalls++
	n := st.initCalls
	cb.mu.Unlock()
	log.Printf("init %s (%s, ID %s) – Grund: %s [Aufruf %d]", i.Name, url, st.id, reason, n)
	_, err := i.Call("init", url, st.id)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	st.lastInit = time.Now()
	st.reason = reason
	if err != nil {
		st.lastErr = err.Error()
		wait := cb.backoffFor(st.fails)
		st.fails++
		st.registered = false
		st.pending = "Wiederholung nach Fehler"
		st.nextTry = time.Now().Add(wait)
		log.Printf("init %s fehlgeschlagen: %v – nächster Versuch in %s", i.Name, err, wait)
		return
	}
	st.registered, st.url, st.lastErr, st.pending = true, url, "", ""
	st.pingSent = time.Time{}
	log.Printf("Events von %s abonniert (%s)", i.Name, url)
}

func (cb *Callback) doDeinit(i *Iface, st *ifState, url, reason string) {
	log.Printf("init %s (%s, leere ID) – Grund: %s", i.Name, url, reason)
	cb.mu.Lock()
	st.deinitCalls++
	st.registered = false
	cb.mu.Unlock()
	if _, err := i.Call("init", url, ""); err != nil {
		log.Printf("Abmeldung bei %s fehlgeschlagen: %v", i.Name, err)
	}
}

// Deregister meldet alle angemeldeten Schnittstellen ab (init mit leerer Interface-ID),
// parallel und höchstens timeout lang. Danach findet kein init mehr statt.
func (cb *Callback) Deregister(timeout time.Duration) {
	cb.mu.Lock()
	cb.stopped = true
	cb.mu.Unlock()
	deadline := time.After(timeout)
	var wg sync.WaitGroup
	for _, i := range cb.ccu.Interfaces() {
		cb.mu.Lock()
		st := cb.states[i.Name]
		cb.mu.Unlock()
		wg.Add(1)
		go func(i *Iface, st *ifState) {
			defer wg.Done()
			select { // laufendes init abwarten
			case st.lock <- struct{}{}:
				defer func() { <-st.lock }()
			case <-time.After(timeout):
				return
			}
			cb.mu.Lock()
			reg, url := st.registered, st.url
			cb.mu.Unlock()
			if reg {
				cb.doDeinit(i, st, url, "Beenden")
			}
		}(i, st)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-deadline:
		log.Printf("Abmeldung nicht vollständig nach %s", timeout)
	}
}

// Status für /api/info: je Schnittstelle Anmeldung, Zähler und Zeiten.
func (cb *Callback) Status() map[string]interface{} {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	out := map[string]interface{}{}
	for _, i := range cb.ccu.Interfaces() {
		st := cb.states[i.Name]
		if st == nil {
			continue
		}
		s := map[string]interface{}{"subscribed": st.registered, "interfaceId": st.id,
			"initCalls": st.initCalls, "deinitCalls": st.deinitCalls, "pings": st.pings}
		if st.url != "" {
			s["callbackUrl"] = st.url
		}
		if !st.lastInit.IsZero() {
			s["lastInit"] = st.lastInit
			s["lastInitReason"] = st.reason
		}
		if !st.lastAlive.IsZero() {
			s["lastEvent"] = st.lastAlive
		}
		if !st.registered && !st.nextTry.IsZero() {
			s["nextInit"] = st.nextTry
		}
		if st.lastErr != "" {
			s["error"] = st.lastErr
		}
		if st.noPing {
			s["ping"] = false
		}
		out[i.Name] = s
	}
	return out
}

// InitCalls liefert die Zahl der init-Aufrufe einer Schnittstelle.
func (cb *Callback) InitCalls(name string) int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if st := cb.states[name]; st != nil {
		return st.initCalls
	}
	return 0
}
