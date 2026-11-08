package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- simulierte CCU-Schnittstelle, die init-Aufrufe zählt ----------

type initCall struct {
	url, id string
	at      time.Time
}

// fakeIface verhält sich wie HMIPServer/rfd: init(url, id) meldet an, init(url, "") ab,
// ping(callerId) schickt ein PONG-Event an den angemeldeten Callback.
type fakeIface struct {
	mu        sync.Mutex
	inits     []initCall
	pings     int
	regURL    string
	regID     string
	noPong    bool          // PONG bleibt aus (Callback nicht erreichbar)
	noPing    bool          // ping unbekannt (Fault)
	initDelay time.Duration // langsames init
}

func (f *fakeIface) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, params, _ := parseRequest(r.Body)
	var res interface{} = ""
	switch method {
	case "system.listMethods":
		res = []interface{}{"init", "ping", "listDevices"}
	case "listDevices":
		res = []interface{}{}
	case "init":
		url, id := params[0].(string), ""
		if len(params) > 1 {
			id, _ = params[1].(string)
		}
		time.Sleep(f.initDelay)
		f.mu.Lock()
		f.inits = append(f.inits, initCall{url, id, time.Now()})
		if id == "" {
			f.regURL, f.regID = "", ""
		} else {
			f.regURL, f.regID = url, id
		}
		f.mu.Unlock()
		if id != "" { // wie die CCU: nach init fragt sie die bekannten Geräte ab
			go func() { _, _ = NewXMLRPCClient(url, 2*time.Second).Call("listDevices", id) }()
		}
	case "ping":
		f.mu.Lock()
		if f.noPing {
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/xml")
			io.WriteString(w, `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-1</i4></value></member><member><name>faultString</name><value>Unknown method</value></member></struct></value></fault></methodResponse>`)
			return
		}
		f.pings++
		url, id, send := f.regURL, f.regID, !f.noPong && f.regURL != ""
		f.mu.Unlock()
		if send {
			caller := params[0]
			go func() { _, _ = NewXMLRPCClient(url, 2*time.Second).Call("event", id, "CENTRAL", "PONG", caller) }()
		}
		res = true
	}
	w.Header().Set("Content-Type", "text/xml")
	_ = writeResponse(w, res)
}

func (f *fakeIface) calls() []initCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]initCall(nil), f.inits...)
}

// withID zählt nur Anmeldungen (nicht Abmeldungen mit leerer ID).
func withID(list []initCall) int {
	n := 0
	for _, c := range list {
		if c.id != "" {
			n++
		}
	}
	return n
}

// forget simuliert einen Neustart des HMIPServers: die Anmeldung ist weg.
func (f *fakeIface) forget() { f.mu.Lock(); f.regURL, f.regID = "", ""; f.mu.Unlock() }

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

type testCCU struct {
	cfg              *Config
	fakes            map[string]*fakeIface
	cux              *fakeCUxD
	xmlAddr, binAddr string
}

// newTestCCU: HmIP-RF, BidCos-RF, VirtualDevices (XML-RPC) und CUxD (BIN-RPC).
func newTestCCU(t *testing.T) *testCCU {
	tc := &testCCU{fakes: map[string]*fakeIface{}, cux: newFakeCUxD(t), xmlAddr: freeAddr(t), binAddr: freeAddr(t)}
	cfg := defaultConfig()
	cfg.APIKey = "geheim"
	cfg.RegaURLs = []string{"http://127.0.0.1:1/rega.exe"}
	cfg.Interfaces = nil
	for _, n := range []string{"HmIP-RF", "BidCos-RF", "VirtualDevices"} {
		f := &fakeIface{}
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		u := srv.URL
		if n == "VirtualDevices" {
			u += "/groups"
		}
		tc.fakes[n] = f
		cfg.Interfaces = append(cfg.Interfaces, InterfaceConfig{Name: n, URLs: []string{u}})
	}
	cfg.Interfaces = append(cfg.Interfaces, InterfaceConfig{Name: "CUxD", URLs: []string{tc.cux.URL()}})
	cfg.CallbackListen, cfg.CallbackURL = tc.xmlAddr, "http://"+tc.xmlAddr
	cfg.BinCallback, cfg.BinCallbackURL = tc.binAddr, "xmlrpc_bin://"+tc.binAddr
	tc.cfg = cfg
	return tc
}

// start entspricht dem Programmstart in main(): Ports öffnen, Callback-Server, Anmeldung.
func (tc *testCCU) start(t *testing.T, tune func(*Callback)) (*Callback, *API, context.CancelFunc) {
	ccu := NewCCU(tc.cfg)
	hub := NewHub()
	cb := NewCallback(ccu, hub, tc.cfg.CallbackListen, tc.cfg.CallbackURL, time.Hour)
	cb.SetBin(tc.cfg.BinCallback, tc.cfg.BinCallbackURL)
	cb.tick = 10 * time.Millisecond
	if tune != nil {
		tune(cb)
	}
	var err error
	for i := 0; i < 50; i++ { // Port des vorherigen Laufs ggf. noch kurz belegt
		if err = cb.Listen(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	go cb.Serve()
	ctx, cancel := context.WithCancel(context.Background())
	go cb.Run(ctx)
	api := &API{cfg: tc.cfg, ccu: ccu, hub: hub, started: time.Now(), cb: cb}
	return cb, api, func() {
		cancel()
		if cb.xmlLn != nil {
			cb.xmlLn.Close()
		}
		if cb.binLn != nil {
			cb.binLn.Close()
		}
	}
}

func (tc *testCCU) cuxInits() []initCall {
	tc.cux.mu.Lock()
	defer tc.cux.mu.Unlock()
	return append([]initCall(nil), tc.cux.inits...)
}

func (tc *testCCU) allInits() map[string][]initCall {
	m := map[string][]initCall{"CUxD": tc.cuxInits()}
	for n, f := range tc.fakes {
		m[n] = f.calls()
	}
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Zeitüberschreitung: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Über Neustart, SSE-Verbindungen, Abfragen und Config-Reload hinweg genau ein init
// je Interface und Prozesslauf; beim Stoppen init mit leerer ID.
func TestInitOncePerInterface(t *testing.T) {
	tc := newTestCCU(t)
	want := map[string]string{"HmIP-RF": "http://" + tc.xmlAddr, "BidCos-RF": "http://" + tc.xmlAddr,
		"VirtualDevices": "http://" + tc.xmlAddr, "CUxD": "xmlrpc_bin://" + tc.binAddr}

	for run := 1; run <= 2; run++ { // zweiter Lauf = Neustart des Add-ons
		cb, api, stop := tc.start(t, nil)
		waitFor(t, "Anmeldung aller Schnittstellen", func() bool {
			for _, l := range tc.allInits() {
				if withID(l) < run {
					return false
				}
			}
			return true
		})

		srv := httptest.NewServer(api.Middleware(api.Routes()))
		// mehrere SSE-Clients kommen und gehen
		for i := 0; i < 5; i++ {
			req, _ := http.NewRequest("GET", srv.URL+"/api/events?apikey=geheim", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 16)
			_, _ = resp.Body.Read(buf)
			resp.Body.Close()
		}
		// normale Anfragen, Gerätelisten, Info
		for i := 0; i < 3; i++ {
			do(t, "GET", srv.URL+"/api/devices", "")
			do(t, "GET", srv.URL+"/api/interfaces", "")
		}
		// Config-Reload mit unveränderten URLs
		cb.SetURLs(tc.cfg.CallbackURL, tc.cfg.BinCallbackURL)
		cb.SetURLs(tc.cfg.CallbackURL, tc.cfg.BinCallbackURL)

		// Callback muss schnell antworten
		t0 := time.Now()
		if _, err := NewXMLRPCClient("http://"+tc.xmlAddr, time.Second).Call("system.multicall", []interface{}{
			map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "X:1", "STATE", true}},
		}); err != nil || time.Since(t0) > 500*time.Millisecond {
			t.Fatalf("Callback zu langsam oder fehlerhaft: %v %s", err, time.Since(t0))
		}

		time.Sleep(200 * time.Millisecond) // viele Watchdog-Durchläufe
		_, m, _ := do(t, "GET", srv.URL+"/api/info", "")
		srv.Close()
		ev := m["events"].(map[string]interface{})
		for name, l := range tc.allInits() {
			if n := withID(l); n != run {
				t.Fatalf("Lauf %d: %s hat %d Anmeldungen erhalten, erwartet %d: %+v", run, name, n, run, l)
			}
			last := l[len(l)-1]
			if last.id != "hmrest-"+name || last.url != want[name] {
				t.Fatalf("%s: init(%q, %q), erwartet init(%q, %q)", name, last.url, last.id, want[name], "hmrest-"+name)
			}
			st := ev[name].(map[string]interface{})
			if st["initCalls"] != float64(1) || st["subscribed"] != true || st["interfaceId"] != "hmrest-"+name {
				t.Fatalf("/api/info %s: %v", name, st)
			}
		}
		ifs := m["interfaces"].([]interface{})
		for _, x := range ifs {
			if x.(map[string]interface{})["initCalls"] != float64(1) {
				t.Fatalf("initCalls fehlt in interfaces: %v", x)
			}
		}

		// Beenden (SIGTERM): Abmeldung mit leerer ID an dieselbe URL
		cb.Deregister(3 * time.Second)
		stop()
		for name, l := range tc.allInits() {
			last := l[len(l)-1]
			if last.id != "" || last.url != want[name] {
				t.Fatalf("%s: keine Abmeldung beim Stoppen: %+v", name, l)
			}
			if len(l) != 2*run {
				t.Fatalf("%s: %d Aufrufe nach Lauf %d: %+v", name, len(l), run, l)
			}
		}
		time.Sleep(50 * time.Millisecond)
		for name, l := range tc.allInits() { // nach dem Stoppen kein init mehr
			if len(l) != 2*run {
				t.Fatalf("%s: init nach dem Stoppen: %+v", name, l)
			}
		}
	}
}

// Watchdog: Solange auf ping ein PONG kommt, kein init. Verliert die Schnittstelle die
// Anmeldung (PONG bleibt aus), folgt genau ein neues init.
func TestWatchdogPingPong(t *testing.T) {
	tc := newTestCCU(t)
	cb, _, stop := tc.start(t, func(cb *Callback) {
		cb.idle, cb.pingWait = 40*time.Millisecond, 80*time.Millisecond
		cb.backoff = []time.Duration{60 * time.Millisecond, 120 * time.Millisecond}
	})
	defer stop()
	hm := tc.fakes["HmIP-RF"]
	waitFor(t, "mehrere pings", func() bool { hm.mu.Lock(); defer hm.mu.Unlock(); return hm.pings >= 4 })
	if n := withID(hm.calls()); n != 1 {
		t.Fatalf("trotz PONG neu angemeldet: %d", n)
	}
	// HMIPServer neu gestartet: Anmeldung weg, PONG bleibt aus → genau ein neues init
	hm.forget()
	waitFor(t, "neue Anmeldung", func() bool { return withID(hm.calls()) == 2 })
	if c := hm.calls(); c[1].id != "hmrest-HmIP-RF" {
		t.Fatalf("neue Anmeldung mit anderer ID: %+v", c)
	}
	p0 := func() int { hm.mu.Lock(); defer hm.mu.Unlock(); return hm.pings }()
	waitFor(t, "pings nach Neuanmeldung", func() bool { hm.mu.Lock(); defer hm.mu.Unlock(); return hm.pings >= p0+4 })
	if n := withID(hm.calls()); n != 2 {
		t.Fatalf("zu viele Anmeldungen: %d", n)
	}
	// CUxD kennt kein ping: wird nur auf Erreichbarkeit geprüft, nie neu angemeldet
	if n := withID(tc.cuxInits()); n != 1 {
		t.Fatalf("CUxD neu angemeldet: %d", n)
	}
	if st := cb.Status()["CUxD"].(map[string]interface{}); st["ping"] != false {
		t.Fatalf("CUxD ohne ping nicht erkannt: %v", st)
	}
}

// Bleibt das PONG nach einem Verlust dauerhaft aus, wird mit wachsendem Abstand neu
// angemeldet – nie in einer Schleife.
func TestReinitBackoff(t *testing.T) {
	tc := newTestCCU(t)
	hm := tc.fakes["HmIP-RF"]
	_, _, stop := tc.start(t, func(cb *Callback) {
		cb.idle, cb.pingWait = 20*time.Millisecond, 20*time.Millisecond
		cb.backoff = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	})
	defer stop()
	waitFor(t, "PONG", func() bool { hm.mu.Lock(); defer hm.mu.Unlock(); return hm.pings >= 2 })
	hm.mu.Lock()
	hm.noPong = true // Callback ab jetzt nicht mehr erreichbar
	hm.mu.Unlock()
	time.Sleep(1300 * time.Millisecond)
	c := hm.calls()
	if n := withID(c); n < 3 || n > 6 {
		t.Fatalf("%d Anmeldungen in 1,3 s (erwartet 3–6 mit Backoff): %+v", n, c)
	}
	for k := 2; k < len(c); k++ {
		if gap := c[k].at.Sub(c[k-1].at); gap < 90*time.Millisecond {
			t.Fatalf("init ohne Backoff (Abstand %s)", gap)
		}
	}
	if gap := c[len(c)-1].at.Sub(c[len(c)-2].at); len(c) >= 4 && gap < 300*time.Millisecond {
		t.Fatalf("Backoff wächst nicht: letzter Abstand %s", gap)
	}
}

// Schnittstelle beantwortet ping, schickt aber nie ein PONG: höchstens ein weiteres init,
// danach Überwachung nur über die Erreichbarkeit.
func TestNeverPong(t *testing.T) {
	tc := newTestCCU(t)
	hm := tc.fakes["HmIP-RF"]
	hm.noPong = true
	cb, _, stop := tc.start(t, func(cb *Callback) {
		cb.idle, cb.pingWait = 20*time.Millisecond, 20*time.Millisecond
		cb.backoff = []time.Duration{50 * time.Millisecond}
	})
	defer stop()
	time.Sleep(800 * time.Millisecond)
	if n := withID(hm.calls()); n > 2 {
		t.Fatalf("%d Anmeldungen ohne PONG", n)
	}
	if st := cb.Status()["HmIP-RF"].(map[string]interface{}); st["ping"] != false || st["subscribed"] != true {
		t.Fatalf("Status falsch: %v", st)
	}
}

// Nicht erreichbare Schnittstelle beim Start: Wiederholung nur mit Backoff.
func TestInitFailureBackoff(t *testing.T) {
	tc := newTestCCU(t)
	tc.cfg.Interfaces = append(tc.cfg.Interfaces, InterfaceConfig{Name: "BidCos-Wired", URLs: []string{"http://127.0.0.1:1"}})
	cb, _, stop := tc.start(t, func(cb *Callback) {
		cb.backoff = []time.Duration{time.Hour}
	})
	defer stop()
	time.Sleep(300 * time.Millisecond)
	if n := cb.InitCalls("BidCos-Wired"); n != 1 {
		t.Fatalf("%d init-Versuche an nicht erreichbare Schnittstelle", n)
	}
	st := cb.Status()["BidCos-Wired"].(map[string]interface{})
	if st["subscribed"] != false || st["nextInit"] == nil || st["error"] == nil {
		t.Fatalf("Status falsch: %v", st)
	}
}

// Pro Schnittstelle nur ein init gleichzeitig.
func TestInitMutex(t *testing.T) {
	tc := newTestCCU(t)
	hm := tc.fakes["HmIP-RF"]
	hm.initDelay = 200 * time.Millisecond
	ccu := NewCCU(tc.cfg)
	cb := NewCallback(ccu, NewHub(), "", tc.cfg.CallbackURL, time.Hour)
	var wg sync.WaitGroup
	for k := 0; k < 10; k++ {
		wg.Add(1)
		go func() { defer wg.Done(); cb.check(ccu.byName["HmIP-RF"]) }()
	}
	wg.Wait()
	for k := 0; k < 5; k++ {
		cb.checkAll()
	}
	time.Sleep(300 * time.Millisecond)
	if n := withID(hm.calls()); n != 1 {
		t.Fatalf("%d gleichzeitige inits", n)
	}
}

// Config-Reload: nur eine geänderte Callback-URL führt zu Ab- und Neuanmeldung.
func TestCallbackURLChange(t *testing.T) {
	tc := newTestCCU(t)
	cb, _, stop := tc.start(t, nil)
	defer stop()
	hm := tc.fakes["HmIP-RF"]
	waitFor(t, "Anmeldung", func() bool { return withID(hm.calls()) == 1 })
	cb.SetURLs(tc.cfg.CallbackURL, tc.cfg.BinCallbackURL)
	time.Sleep(100 * time.Millisecond)
	if len(hm.calls()) != 1 {
		t.Fatalf("init trotz unveränderter URL: %+v", hm.calls())
	}
	newURL := "http://127.0.0.1:19126"
	cb.SetURLs(newURL, tc.cfg.BinCallbackURL)
	waitFor(t, "Neuanmeldung", func() bool { return len(hm.calls()) == 3 })
	time.Sleep(100 * time.Millisecond)
	c := hm.calls()
	if len(c) != 3 || c[1].id != "" || c[1].url != tc.cfg.CallbackURL || c[2].id != "hmrest-HmIP-RF" || c[2].url != newURL {
		t.Fatalf("falsche Abfolge: %+v", c)
	}
	if n := withID(tc.cuxInits()); n != 1 { // BIN-URL unverändert → CUxD bleibt angemeldet
		t.Fatalf("CUxD neu angemeldet: %d", n)
	}
	if !strings.HasPrefix(cb.Status()["HmIP-RF"].(map[string]interface{})["lastInitReason"].(string), "Callback-URL geändert") {
		t.Fatal("Grund fehlt")
	}
}
