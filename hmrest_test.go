package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- simulierte CCU ----------

type fakeCCU struct {
	mu      sync.Mutex
	values  map[string]map[string]interface{}
	calls   []string
	lastSet []interface{}
}

func newFake() *fakeCCU {
	return &fakeCCU{values: map[string]map[string]interface{}{
		"000A1B2C3D4E5F:1": {"LEVEL": 0.25, "ACTIVITY_STATE": int64(0)},
		"000A1B2C3D4E5F:0": {"UNREACH": false, "OPERATING_VOLTAGE": 3.1},
	}}
}

var fakeDevices = []interface{}{
	map[string]interface{}{"ADDRESS": "000A1B2C3D4E5F", "TYPE": "HmIP-BROLL", "FIRMWARE": "2.2.4",
		"PARAMSETS": []interface{}{"MASTER", "SERVICE"}, "CHILDREN": []interface{}{"000A1B2C3D4E5F:0", "000A1B2C3D4E5F:1"}},
	map[string]interface{}{"ADDRESS": "000A1B2C3D4E5F:0", "TYPE": "MAINTENANCE", "PARENT": "000A1B2C3D4E5F", "INDEX": int64(0),
		"PARAMSETS": []interface{}{"MASTER", "VALUES"}},
	map[string]interface{}{"ADDRESS": "000A1B2C3D4E5F:1", "TYPE": "BLIND_VIRTUAL_RECEIVER", "PARENT": "000A1B2C3D4E5F", "INDEX": int64(1),
		"PARAMSETS": []interface{}{"MASTER", "VALUES"}},
}

func (f *fakeCCU) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, params, err := parseRequest(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	var res interface{} = ""
	switch method {
	case "system.listMethods":
		res = []interface{}{"listDevices"}
	case "listDevices":
		res = fakeDevices
	case "getParamsetDescription":
		addr := params[0].(string)
		if strings.HasSuffix(addr, ":1") {
			res = map[string]interface{}{
				"LEVEL":          map[string]interface{}{"TYPE": "FLOAT", "MIN": 0.0, "MAX": 1.01, "OPERATIONS": int64(7), "UNIT": "100%"},
				"STOP":           map[string]interface{}{"TYPE": "ACTION", "OPERATIONS": int64(2)},
				"ACTIVITY_STATE": map[string]interface{}{"TYPE": "ENUM", "MIN": int64(0), "MAX": int64(3), "OPERATIONS": int64(5), "VALUE_LIST": []interface{}{"UNKNOWN", "UP", "DOWN", "STABLE"}},
				"DURATION_UNIT":  map[string]interface{}{"TYPE": "ENUM", "MIN": int64(0), "MAX": int64(2), "OPERATIONS": int64(2), "VALUE_LIST": []interface{}{"S", "M", "H"}},
			}
		} else {
			res = map[string]interface{}{
				"UNREACH":           map[string]interface{}{"TYPE": "BOOL", "OPERATIONS": int64(5)},
				"OPERATING_VOLTAGE": map[string]interface{}{"TYPE": "FLOAT", "OPERATIONS": int64(5), "UNIT": "V"},
			}
		}
	case "getParamset":
		res = f.values[params[0].(string)]
	case "getValue":
		res = f.values[params[0].(string)][params[1].(string)]
	case "setValue":
		f.lastSet = params
		if params[1] == "LEVEL" {
			f.values[params[0].(string)]["LEVEL"] = params[2]
		}
	case "putParamset":
		f.lastSet = params
	case "init":
	default:
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-1</i4></value></member><member><name>faultString</name><value>Unknown method</value></member></struct></value></fault></methodResponse>`)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	_ = writeResponse(w, res)
}

func fakeRega(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := string(body)
	out := ""
	switch {
	case strings.Contains(s, "root.Devices()"):
		out = "D\t1234\t000A1B2C3D4E5F\tRollladen Küche\nC\t1235\t000A1B2C3D4E5F:1\tRollladen Küche:1\nr\t${roomKitchen}\nR\t${roomKitchen}\t000A1B2C3D4E5F:1\n"
	case strings.Contains(s, "ID_SYSTEM_VARIABLES"):
		out = "V\t950\tAnwesenheit\t2\t2\t\t\t2026-10-11 08:00:00\t\t\tabwesend\tanwesend\tfalse\n" +
			"V\t951\tModus\t16\t29\tAus;Tag;Nacht\t\t2026-10-11 08:00:00\t\t\t\t\t1\n"
	case strings.Contains(s, "ID_PROGRAMS"):
		out = "P\t1500\ttrue\ttrue\t2026-10-11 07:00:00\tMorgens hoch\n"
	case strings.Contains(s, ".State("):
		out = "OK"
	}
	w.Write(utf8ToLatin1([]byte(out + "<xml><exec>/rega.exe</exec></xml>")))
}

func setup(t *testing.T) (*httptest.Server, *fakeCCU, *API) {
	fake := newFake()
	ccuSrv := httptest.NewServer(fake)
	regaSrv := httptest.NewServer(http.HandlerFunc(fakeRega))
	t.Cleanup(func() { ccuSrv.Close(); regaSrv.Close() })
	cfg := defaultConfig()
	cfg.APIKey = "geheim"
	cfg.RegaURLs = []string{"http://127.0.0.1:1/rega.exe", regaSrv.URL}
	cfg.Interfaces = []InterfaceConfig{{Name: "HmIP-RF", URLs: []string{"http://127.0.0.1:1", ccuSrv.URL}}}
	hub := NewHub()
	ccu := NewCCU(cfg)
	api := &API{cfg: cfg, ccu: ccu, hub: hub, started: time.Now()}
	api.cb = NewCallback(ccu, hub, "", "http://x", time.Minute)
	srv := httptest.NewServer(api.Middleware(api.Routes()))
	t.Cleanup(srv.Close)
	return srv, fake, api
}

func do(t *testing.T, method, url, body string) (int, map[string]interface{}, []interface{}) {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("X-API-Key", "geheim")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	var a []interface{}
	if json.Unmarshal(b, &m) != nil {
		json.Unmarshal(b, &a)
	}
	t.Logf("%s %s -> %d %s", method, url, resp.StatusCode, strings.TrimSpace(string(b)))
	return resp.StatusCode, m, a
}

func TestXMLRPCRoundtrip(t *testing.T) {
	var b bytes.Buffer
	in := map[string]interface{}{"a": int64(5), "b": 1.5, "c": true, "d": "Grüße <&>", "e": []interface{}{int64(1), "x"}}
	writeResponse(&b, in)
	out, err := parseResponse(&b)
	if err != nil {
		t.Fatal(err)
	}
	o := out.(map[string]interface{})
	if o["a"] != int64(5) || o["b"] != 1.5 || o["c"] != true || o["d"] != "Grüße <&>" || len(o["e"].([]interface{})) != 2 {
		t.Fatalf("falsch: %#v", o)
	}
}

func TestAuth(t *testing.T) {
	srv, _, _ := setup(t)
	resp, _ := http.Get(srv.URL + "/api/info")
	if resp.StatusCode != 401 {
		t.Fatalf("erwartet 401, bekam %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/api/info?apikey=geheim")
	if resp.StatusCode != 200 {
		t.Fatalf("erwartet 200, bekam %d", resp.StatusCode)
	}
}

func TestDevicesAndValues(t *testing.T) {
	srv, fake, _ := setup(t)
	code, _, list := do(t, "GET", srv.URL+"/api/devices?values=1", "")
	if code != 200 || len(list) != 1 {
		t.Fatalf("Geräteliste falsch")
	}
	d := list[0].(map[string]interface{})
	if d["name"] != "Rollladen Küche" {
		t.Fatalf("Name fehlt: %v", d["name"])
	}
	chans := d["channels"].([]interface{})
	c1 := chans[1].(map[string]interface{})
	if c1["values"].(map[string]interface{})["LEVEL"] != 0.25 || c1["rooms"].([]interface{})[0] != "Küche" {
		t.Fatalf("Kanal falsch: %v", c1)
	}

	code, m, _ := do(t, "GET", srv.URL+"/api/devices/000a1b2c3d4e5f:1/values/LEVEL", "")
	if code != 200 || m["value"] != 0.25 || m["writable"] != true {
		t.Fatal("getValue falsch")
	}

	// Prozentwert als String mit Komma -> double
	code, m, _ = do(t, "PUT", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values/LEVEL", `{"value":"0,5"}`)
	if code != 200 || fake.lastSet[2] != 0.5 {
		t.Fatalf("setValue LEVEL falsch: %v", fake.lastSet)
	}
	// ACTION per Query
	code, _, _ = do(t, "POST", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values/STOP?value=1", "")
	if code != 200 || fake.lastSet[2] != true {
		t.Fatalf("ACTION falsch: %v", fake.lastSet)
	}
	// ENUM per Name -> int
	code, _, _ = do(t, "PUT", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values/DURATION_UNIT", `"M"`)
	if code != 200 || fake.lastSet[2] != int64(1) {
		t.Fatalf("ENUM falsch: %#v", fake.lastSet)
	}
	// Bereich prüfen
	if code, _, _ = do(t, "PUT", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values/LEVEL", `2`); code != 400 {
		t.Fatal("Bereichsprüfung fehlt")
	}
	// nur lesbar
	if code, _, _ = do(t, "PUT", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values/ACTIVITY_STATE", `1`); code != 403 {
		t.Fatal("Schreibschutz fehlt")
	}
	// unbekannte Adresse
	if code, _, _ = do(t, "GET", srv.URL+"/api/devices/XYZ:1/values", ""); code != 404 {
		t.Fatal("404 erwartet")
	}
	// putParamset
	code, _, _ = do(t, "PUT", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values", `{"LEVEL":1,"STOP":"true"}`)
	if code != 200 || fake.lastSet[1] != "VALUES" {
		t.Fatalf("putParamset falsch: %v", fake.lastSet)
	}
	// Gerät mit Meta
	code, m, _ = do(t, "GET", srv.URL+"/api/devices/000A1B2C3D4E5F?values=full", "")
	if code != 200 || len(m["channels"].([]interface{})) != 2 {
		t.Fatal("Gerätedetail falsch")
	}
}

func TestSysvarsPrograms(t *testing.T) {
	srv, _, _ := setup(t)
	_, _, list := do(t, "GET", srv.URL+"/api/sysvars", "")
	if len(list) != 2 {
		t.Fatal("Systemvariablen fehlen")
	}
	code, _, _ := do(t, "PUT", srv.URL+"/api/sysvars/Anwesenheit", `{"value":"anwesend"}`)
	if code != 200 {
		t.Fatal("Systemvariable setzen fehlgeschlagen")
	}
	code, _, _ = do(t, "PUT", srv.URL+"/api/sysvars/Modus", `{"value":"Nacht"}`)
	if code != 200 {
		t.Fatal("Enum-Systemvariable setzen fehlgeschlagen")
	}
	_, _, list = do(t, "GET", srv.URL+"/api/programs", "")
	if len(list) != 1 {
		t.Fatal("Programme fehlen")
	}
	if code, _, _ = do(t, "POST", srv.URL+"/api/programs/Morgens%20hoch/run", ""); code != 200 {
		t.Fatal("Programmstart fehlgeschlagen")
	}
	_, _, list = do(t, "GET", srv.URL+"/api/rooms", "")
	if len(list) != 1 {
		t.Fatal("Räume fehlen")
	}
}

func TestEvents(t *testing.T) {
	srv, _, api := setup(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/events?apikey=geheim&param=LEVEL", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	// so ruft die CCU den Callback auf
	api.cb.dispatch("system.multicall", []interface{}{[]interface{}{
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "ACTIVITY_STATE", int64(1)}},
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "LEVEL", 0.75}},
	}})
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "data: ") {
			var e Event
			json.Unmarshal([]byte(l[6:]), &e)
			if e.Param != "LEVEL" || e.Value != 0.75 || e.Interface != "HmIP-RF" {
				t.Fatalf("Event falsch: %+v", e)
			}
			return
		}
	}
	t.Fatal("kein Event empfangen")
}

// fakeWebUI simuliert JSON-API und ReGa der CCU mit genau einer gültigen Sitzung.
const goodSID = "AbCdE12345"

func fakeWebUI(t *testing.T, jsonWorks, regaWorks bool) (jsonURL, regaURL string) {
	js := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params map[string]string `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if jsonWorks && req.Method == "Session.renewSession" && req.Params["_session_id_"] == goodSID {
			io.WriteString(w, `{"version":"1.1","result":true,"error":null}`)
			return
		}
		io.WriteString(w, `{"version":"1.1","result":null,"error":{"name":"JSONRPCError","code":400,"message":"access denied ( invalid session id )"}}`)
	}))
	rg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		out := ""
		if regaWorks && strings.Contains(string(b), "GetSessionVarStr('"+goodSID+"')") {
			out = "1;admin"
		}
		io.WriteString(w, out+"<xml><exec>/rega.exe</exec></xml>")
	}))
	t.Cleanup(func() { js.Close(); rg.Close() })
	return js.URL + "/api/homematic.cgi", rg.URL + "/rega.exe"
}

func TestSessionChecker(t *testing.T) {
	for _, tc := range []struct{ jsonOK, regaOK bool }{{true, false}, {false, true}, {true, true}} {
		j, r := fakeWebUI(t, tc.jsonOK, tc.regaOK)
		// erste URL nicht erreichbar -> Fallback testen
		sc := NewSessionChecker([]string{"http://127.0.0.1:1/x", j}, NewRega([]string{r}))
		for _, in := range []string{"@" + goodSID + "@", goodSID, "%40" + goodSID + "%40"} {
			if !sc.Valid(in) {
				t.Fatalf("%+v: gültige Sitzung %q abgelehnt", tc, in)
			}
		}
		for _, in := range []string{"", "@ZZZZZZZZZZ@", "@abc@", "@" + goodSID + "'@", "x'); dom.X('"} {
			if sc.Valid(in) {
				t.Fatalf("%+v: ungültige Sitzung %q akzeptiert", tc, in)
			}
		}
	}
	// beide Wege liefern nichts -> ungültig
	j, r := fakeWebUI(t, false, false)
	if NewSessionChecker([]string{j}, NewRega([]string{r})).Valid(goodSID) {
		t.Fatal("Sitzung ohne Bestätigung akzeptiert")
	}
}

func TestSetupKey(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Interfaces = nil
	j, r := fakeWebUI(t, true, false)
	cfg.RegaURLs = []string{r}
	ccu := NewCCU(cfg)
	api := &API{cfg: cfg, cfgPath: dir + "/config.json", ccu: ccu, hub: NewHub(), started: time.Now(),
		sessions: NewSessionChecker([]string{j}, ccu.Rega)}
	srv := httptest.NewServer(api.Middleware(api.Routes()))
	defer srv.Close()
	var k map[string]string
	call := func(method, path string, hdr map[string]string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		for a, b := range hdr {
			req.Header.Set(a, b)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		k = map[string]string{}
		json.NewDecoder(resp.Body).Decode(&k)
		return resp.StatusCode
	}

	// ohne Key: API gesperrt, Status meldet fehlenden Key
	if c := call("GET", "/api/info", nil); c != 401 {
		t.Fatalf("erwartet 401, bekam %d", c)
	}
	// anonym: weder abholen noch erzeugen
	if c := call("POST", "/setup/apikey", nil); c != 401 {
		t.Fatalf("anonyme Key-Erzeugung muss 401 sein, war %d", c)
	}
	if c := call("GET", "/setup/apikey", nil); c != 401 {
		t.Fatalf("anonymes Abholen muss 401 sein, war %d", c)
	}
	if c := call("GET", "/setup/apikey?sid=@ZZZZZZZZZZ@", nil); c != 401 {
		t.Fatalf("falsche Sitzung muss 401 sein, war %d", c)
	}
	// mit Sitzung abholen -> wird bei Ersteinrichtung erzeugt
	if c := call("GET", "/setup/apikey", map[string]string{"X-HM-Session": "@" + goodSID + "@"}); c != 200 || len(k["apiKey"]) != 32 {
		t.Fatalf("Abholen mit Sitzung fehlgeschlagen: %d %v", c, k)
	}
	key1 := k["apiKey"]
	if c2, err := loadConfig(dir + "/config.json"); err != nil || c2.APIKey != key1 {
		t.Fatal("Key nicht gespeichert")
	}
	// erneutes Abholen liefert denselben Key
	call("GET", "/setup/apikey?sid=%40"+goodSID+"%40", nil)
	if k["apiKey"] != key1 {
		t.Fatal("Abholen hat den Key verändert")
	}
	// neuer Key mit Sitzung
	if c := call("POST", "/setup/apikey?sid=@"+goodSID+"@", nil); c != 200 || k["apiKey"] == key1 {
		t.Fatal("Erneuern mit Sitzung fehlgeschlagen")
	}
	key2 := k["apiKey"]
	// neuer Key mit aktuellem Key (ohne Sitzung)
	if c := call("POST", "/setup/apikey", map[string]string{"X-API-Key": key2}); c != 200 || k["apiKey"] == key2 {
		t.Fatal("Erneuern mit Key fehlgeschlagen")
	}
	key3 := k["apiKey"]
	// mit altem Key nicht mehr
	if c := call("POST", "/setup/apikey", map[string]string{"X-API-Key": key1}); c != 401 {
		t.Fatalf("alter Key darf nicht erneuern (%d)", c)
	}
	if c := call("GET", "/api/info?apikey="+key2, nil); c != 401 {
		t.Fatal("alter Key noch gültig")
	}
	if c := call("GET", "/api/info?apikey="+key3, nil); c != 200 {
		t.Fatal("neuer Key ungültig")
	}
}

// Gemeldeter Fehler: "" statt Zahl im Event-Stream brachte die HA-Integration zum Absturz.
func TestEmptyValuesAreFiltered(t *testing.T) {
	srv, fake, api := setup(t)
	// Geräteliste laden (Beschreibung wird für die Prüfung gebraucht)
	do(t, "GET", srv.URL+"/api/devices", "")

	req, _ := http.NewRequest("GET", srv.URL+"/api/events?apikey=geheim", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	api.cb.dispatch("system.multicall", []interface{}{[]interface{}{
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "LEVEL", ""}},
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "LEVEL", "  "}},
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "ACTIVITY_STATE", "2"}},
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "000A1B2C3D4E5F:1", "LEVEL", "0,4"}},
		map[string]interface{}{"methodName": "event", "params": []interface{}{"hmrest-HmIP-RF", "CENTRAL:0", "PONG", "hmrest"}},
	}})
	var got []Event
	sc := bufio.NewScanner(resp.Body)
	for len(got) < 3 && sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "data: ") {
			var e Event
			json.Unmarshal([]byte(l[6:]), &e)
			got = append(got, e)
		}
	}
	if len(got) != 3 || got[0].Param != "ACTIVITY_STATE" || got[0].Value != float64(2) ||
		got[1].Param != "LEVEL" || got[1].Value != 0.4 || got[2].Param != "PONG" {
		t.Fatalf("Events falsch gefiltert/gewandelt: %+v", got)
	}

	// auch die Werteabfrage (Resync der Integration) darf kein "" für Zahlen liefern
	fake.mu.Lock()
	fake.values["000A1B2C3D4E5F:1"]["LEVEL"] = ""
	fake.mu.Unlock()
	_, m, _ := do(t, "GET", srv.URL+"/api/devices/000A1B2C3D4E5F:1/values", "")
	if _, has := m["LEVEL"].(map[string]interface{})["value"]; has {
		t.Fatalf("leerer LEVEL-Wert wurde ausgeliefert: %v", m["LEVEL"])
	}
	_, _, list := do(t, "GET", srv.URL+"/api/devices?values=1", "")
	ch := list[0].(map[string]interface{})["channels"].([]interface{})[1].(map[string]interface{})
	if _, has := ch["values"].(map[string]interface{})["LEVEL"]; has {
		t.Fatal("leerer LEVEL-Wert in der Geräteliste")
	}
}
