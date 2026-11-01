package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBinDouble(t *testing.T) {
	for _, f := range []float64{0, 1, -1, 0.5, 0.25, 1.01, 21.5, -12.3, 100, 1e6, 3.14159, 0.001, 65535} {
		m, e := binEncodeDouble(f)
		got := binDecodeDouble(m, e)
		if math.Abs(got-f) > math.Abs(f)*1e-8+1e-12 {
			t.Fatalf("%v -> (%d,%d) -> %v", f, m, e, got)
		}
		if m >= 0x40000000 || m <= -0x40000000 {
			t.Fatalf("Mantisse außerhalb: %v -> %d", f, m)
		}
	}
	// bekannter Wert aus der CCU-Welt: 1.0 = Mantisse 0x20000000, Exponent 1
	if m, e := binEncodeDouble(1.0); m != 0x20000000 || e != 1 {
		t.Fatalf("1.0 falsch kodiert: %X %d", m, e)
	}
}

func TestBinRoundtrip(t *testing.T) {
	in := []interface{}{int64(-5), true, "Grüße", 21.5,
		[]interface{}{int64(1), "x"},
		map[string]interface{}{"A": int64(1), "B": map[string]interface{}{"C": false}}}
	req, err := binEncodeRequest("test.method", in)
	if err != nil {
		t.Fatal(err)
	}
	typ, body, err := binReadMessage(bytes.NewReader(req))
	if err != nil || typ != 0 {
		t.Fatal(err, typ)
	}
	m, p, err := binParseRequest(body)
	if err != nil || m != "test.method" || len(p) != len(in) {
		t.Fatalf("%v %q %v", err, m, p)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(in)
	if string(a) != string(b) {
		t.Fatalf("Rundreise falsch:\n%s\n%s", a, b)
	}
	// Fehlerantwort
	var pl bytes.Buffer
	binEncodeValue(&pl, map[string]interface{}{"faultCode": int64(-2), "faultString": "Unknown instance"})
	typ, body, _ = binReadMessage(bytes.NewReader(binFrame(0xFF, pl.Bytes())))
	if _, err := binParseResponse(typ, body); !isFault(err) {
		t.Fatalf("Fault nicht erkannt: %v", err)
	}
	// kaputte Daten dürfen nicht abstürzen
	for _, junk := range [][]byte{[]byte("Bin\x00\x00\x00\x00\x08\x00\x00\x01\x00\xff\xff\xff\xff"), []byte("XYZ\x00\x00\x00\x00\x00"), []byte("Bin\x00\xff\xff\xff\xff")} {
		if typ, body, err := binReadMessage(bytes.NewReader(junk)); err == nil {
			binParseRequest(body)
			binParseResponse(typ, body)
		}
	}
}

// ---------- simulierter CUxD (BIN-RPC auf TCP) ----------

type fakeCUxD struct {
	ln      net.Listener
	mu      sync.Mutex
	state   bool
	lastSet []interface{}
	initURL string
	initID  string
}

func newFakeCUxD(t *testing.T) *fakeCUxD {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCUxD{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return f
}

func (f *fakeCUxD) URL() string { return "bin://" + f.ln.Addr().String() }

func (f *fakeCUxD) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	typ, body, err := binReadMessage(br)
	if err != nil || typ != 0 {
		return
	}
	method, params, _ := binParseRequest(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	var res interface{} = ""
	switch method {
	case "listDevices":
		res = []interface{}{
			map[string]interface{}{"ADDRESS": "CUX2801001", "TYPE": "HM-LC-Sw1-Pl-CT", "PARAMSETS": []interface{}{"MASTER"},
				"CHILDREN": []interface{}{"CUX2801001:1"}, "PARENT": "", "FIRMWARE": "1.0"},
			map[string]interface{}{"ADDRESS": "CUX2801001:1", "TYPE": "SWITCH", "PARAMSETS": []interface{}{"MASTER", "VALUES"},
				"PARENT": "CUX2801001", "INDEX": int64(1)},
		}
	case "getParamsetDescription":
		res = map[string]interface{}{
			"STATE":  map[string]interface{}{"TYPE": "BOOL", "OPERATIONS": int64(7)},
			"POWER":  map[string]interface{}{"TYPE": "FLOAT", "OPERATIONS": int64(5), "UNIT": "W", "MIN": 0.0, "MAX": 3680.0},
			"SYSLOG": map[string]interface{}{"TYPE": "STRING", "OPERATIONS": int64(2)},
		}
	case "getParamset":
		res = map[string]interface{}{"STATE": f.state, "POWER": 12.75}
	case "getValue":
		res = f.state
	case "setValue":
		f.lastSet = params
		if params[1] == "STATE" {
			f.state, _ = params[2].(bool)
		}
	case "init":
		f.initURL = params[0].(string)
		if len(params) > 1 {
			f.initID = params[1].(string)
		}
	default:
		var pl bytes.Buffer
		binEncodeValue(&pl, map[string]interface{}{"faultCode": int64(-1), "faultString": "unknown method"})
		c.Write(binFrame(0xFF, pl.Bytes()))
		return
	}
	out, _ := binEncodeResponse(res)
	c.Write(out)
}

func TestCUxD(t *testing.T) {
	cux := newFakeCUxD(t)
	cfg := defaultConfig()
	cfg.APIKey = "geheim"
	cfg.RegaURLs = []string{"http://127.0.0.1:1/rega.exe"}
	cfg.Interfaces = []InterfaceConfig{{Name: "CUxD", URLs: []string{"bin://127.0.0.1:1", cux.URL()}}}
	ccu := NewCCU(cfg)
	hub := NewHub()
	api := &API{cfg: cfg, ccu: ccu, hub: hub, started: time.Now()}
	srv := httptest.NewServer(api.Middleware(api.Routes()))
	defer srv.Close()

	code, _, list := do(t, "GET", srv.URL+"/api/devices?values=1", "")
	if code != 200 || len(list) != 1 {
		t.Fatalf("CUxD-Geräte nicht gefunden: %d %v", code, list)
	}
	d := list[0].(map[string]interface{})
	ch := d["channels"].([]interface{})[0].(map[string]interface{})
	vals := ch["values"].(map[string]interface{})
	if d["interface"] != "CUxD" || vals["POWER"] != 12.75 || vals["STATE"] != false {
		t.Fatalf("CUxD-Werte falsch: %v", d)
	}
	code, _, _ = do(t, "PUT", srv.URL+"/api/devices/CUX2801001:1/values/STATE", `{"value":"on"}`)
	if code != 200 || cux.lastSet[2] != true {
		t.Fatalf("Schalten über CUxD fehlgeschlagen: %v", cux.lastSet)
	}
	code, m, _ := do(t, "GET", srv.URL+"/api/devices/CUX2801001:1/values/STATE", "")
	if code != 200 || m["value"] != true {
		t.Fatal("Wert nach Schalten falsch")
	}
	code, m, _ = do(t, "GET", srv.URL+"/api/info", "")
	ifs := m["interfaces"].([]interface{})[0].(map[string]interface{})
	if ifs["available"] != true || ifs["url"] != cux.URL() {
		t.Fatalf("Status falsch: %v", ifs)
	}

	// Events: Anmeldung per xmlrpc_bin:// und Empfang über den BIN-RPC-Callback
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	cb := NewCallback(ccu, hub, "127.0.0.1:0", "http://unused", time.Minute)
	cb.SetBin(addr, "xmlrpc_bin://"+addr)
	go cb.ListenAndServe()
	ctx, cancel := contextWithCancel()
	defer cancel()
	go cb.Run(ctx)
	sub := hub.Subscribe()
	deadline := time.Now().Add(3 * time.Second)
	for {
		cux.mu.Lock()
		u, id := cux.initURL, cux.initID
		cux.mu.Unlock()
		if u != "" {
			if u != "xmlrpc_bin://"+addr || id != "hmrest-CUxD" {
				t.Fatalf("init falsch: %q %q", u, id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("init wurde nicht aufgerufen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// so meldet CUxD Änderungen: system.multicall über BIN-RPC
	cl := NewBinRPCClient("xmlrpc_bin://"+addr, 3*time.Second)
	var res interface{}
	var err error
	for i := 0; i < 50; i++ { // Server braucht ggf. einen Moment
		res, err = cl.Call("system.multicall", []interface{}{
			map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-CUxD", "CUX2801001:1", "POWER", 230.5}},
		})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Callback nicht erreichbar: %v", err)
	}
	if arr, ok := res.([]interface{}); !ok || len(arr) != 1 {
		t.Fatalf("multicall-Antwort falsch: %#v", res)
	}
	select {
	case e := <-sub:
		if e.Interface != "CUxD" || e.Param != "POWER" || e.Value != 230.5 {
			t.Fatalf("Event falsch: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kein Event empfangen")
	}
	// listDevices vom Callback: leere Liste
	if r, err := cl.Call("listDevices", "hmrest-CUxD"); err != nil || len(r.([]interface{})) != 0 {
		t.Fatalf("listDevices: %v %v", r, err)
	}
}

func TestConfigMigration(t *testing.T) {
	p := t.TempDir() + "/config.json"
	old := `{"apiKey":"abc","interfaces":[{"name":"HmIP-RF","urls":["http://127.0.0.1:2010"]},{"name":"BidCos-RF","urls":["http://x"],"enabled":false}]}`
	os.WriteFile(p, []byte(old), 0600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, i := range cfg.Interfaces {
		names = append(names, i.Name)
	}
	j := strings.Join(names, ",")
	if !strings.Contains(j, "CUxD") || strings.Count(j, "BidCos-RF") != 1 || cfg.APIKey != "abc" {
		t.Fatalf("Migration falsch: %s", j)
	}
	// abgeschaltete Schnittstelle bleibt abgeschaltet
	if c := NewCCU(cfg); c.byName["BidCos-RF"] != nil || c.byName["CUxD"] == nil {
		t.Fatal("enabled:false nicht beachtet")
	}
	// gespeichert, zweiter Lauf ändert nichts mehr
	b1, _ := os.ReadFile(p)
	loadConfig(p)
	b2, _ := os.ReadFile(p)
	if !strings.Contains(string(b1), "bin://127.0.0.1:8701") || string(b1) != string(b2) {
		t.Fatal("Migration nicht stabil gespeichert")
	}
}

var _ = http.StatusOK

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// Genau der gemeldete Fall: CUxD mit http:// eingetragen -> CUxD setzt die Verbindung zurück.
// Das Add-on muss BIN-RPC selbst erkennen.
func TestCUxDWithHTTPURL(t *testing.T) {
	cux := newFakeCUxD(t)
	httpURL := "http://" + cux.ln.Addr().String()

	// erst prüfen, dass XML-RPC wirklich scheitert (wie auf der CCU)
	if _, err := NewXMLRPCClient(httpURL, 3*time.Second).Call("listDevices"); err == nil || isFault(err) {
		t.Fatalf("XML-RPC hätte scheitern müssen: %v", err)
	}
	cfg := defaultConfig()
	cfg.APIKey = "geheim"
	cfg.RegaURLs = []string{"http://127.0.0.1:1/rega.exe"}
	cfg.Interfaces = []InterfaceConfig{{Name: "Eigene", URLs: []string{httpURL}}}
	ccu := NewCCU(cfg)
	api := &API{cfg: cfg, ccu: ccu, hub: NewHub(), started: time.Now()}
	srv := httptest.NewServer(api.Middleware(api.Routes()))
	defer srv.Close()
	code, _, list := do(t, "GET", srv.URL+"/api/devices", "")
	if code != 200 || len(list) != 1 {
		t.Fatalf("Geräte über erkannte BIN-RPC-Verbindung fehlen: %v", list)
	}
	if !ccu.byName["Eigene"].IsBin() {
		t.Fatal("IsBin nach Erkennung falsch – Events würden per XML-RPC angemeldet")
	}
	_, m, _ := do(t, "GET", srv.URL+"/api/info", "")
	st := m["interfaces"].([]interface{})[0].(map[string]interface{})
	if st["available"] != true || !strings.HasPrefix(st["url"].(string), "bin://") {
		t.Fatalf("Status falsch: %v", st)
	}
}

func TestConfigFixCUxDURL(t *testing.T) {
	p := t.TempDir() + "/config.json"
	os.WriteFile(p, []byte(`{"apiKey":"k","interfaces":[
		{"name":"CUxD","urls":["http://127.0.0.1:8701"]},
		{"name":"Andere","urls":["http://127.0.0.1:8701/"]},
		{"name":"HmIP-RF","urls":["http://127.0.0.1:2010"]}]}`), 0600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, i := range cfg.Interfaces {
		got[i.Name] = strings.Join(i.URLs, ",")
	}
	if got["CUxD"] != "bin://127.0.0.1:8701" || got["Andere"] != "bin://127.0.0.1:8701" || got["HmIP-RF"] != "http://127.0.0.1:2010" {
		t.Fatalf("Korrektur falsch: %v", got)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "http://127.0.0.1:8701") {
		t.Fatal("Korrektur nicht gespeichert")
	}
}
