package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeThermostat simuliert ein HM-CC-RT-DN (Programm im Geräte-MASTER).
type fakeThermostat struct {
	mu       sync.Mutex
	prefixed bool
	master   map[string]interface{}
	puts     int
}

func newFakeThermostat(prefixed bool) *fakeThermostat {
	f := &fakeThermostat{prefixed: prefixed, master: map[string]interface{}{}}
	for _, p := range f.profiles() {
		for _, d := range weekDays {
			for n := 1; n <= 13; n++ {
				end, t := 1440, 17.0
				if n == 1 {
					end = 360
				} else if n == 2 {
					end, t = 1320, 21.0
				}
				if n > 2 {
					t = 17.0
				}
				f.master[f.name(p, "ENDTIME", d, n)] = int64(end)
				f.master[f.name(p, "TEMPERATURE", d, n)] = t
			}
		}
	}
	if prefixed {
		f.master["WEEK_PROGRAM_POINTER"] = int64(0)
	}
	f.master["BOOST_TIME_PERIOD"] = int64(3)
	return f
}

func (f *fakeThermostat) profiles() []int {
	if f.prefixed {
		return []int{1, 2, 3}
	}
	return []int{1}
}

func (f *fakeThermostat) name(p int, kind, day string, n int) string {
	if f.prefixed {
		return fmt.Sprintf("P%d_%s_%s_%d", p, kind, day, n)
	}
	return fmt.Sprintf("%s_%s_%d", kind, day, n)
}

func (f *fakeThermostat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, params, _ := parseRequest(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	var res interface{} = ""
	switch method {
	case "system.listMethods":
		res = []interface{}{"listDevices"}
	case "listDevices":
		res = []interface{}{
			map[string]interface{}{"ADDRESS": "MEQ0000001", "TYPE": "HM-CC-RT-DN", "PARAMSETS": []interface{}{"MASTER"},
				"CHILDREN": []interface{}{"MEQ0000001:0", "MEQ0000001:4"}},
			map[string]interface{}{"ADDRESS": "MEQ0000001:0", "TYPE": "MAINTENANCE", "PARENT": "MEQ0000001", "INDEX": int64(0), "PARAMSETS": []interface{}{"MASTER", "VALUES"}},
			map[string]interface{}{"ADDRESS": "MEQ0000001:4", "TYPE": "CLIMATECONTROL_RT_TRANSCEIVER", "PARENT": "MEQ0000001", "INDEX": int64(4), "PARAMSETS": []interface{}{"MASTER", "VALUES"}},
		}
	case "getParamsetDescription":
		addr, key := params[0].(string), params[1].(string)
		d := map[string]interface{}{}
		if addr == "MEQ0000001" && key == "MASTER" {
			for name := range f.master {
				switch {
				case strings.Contains(name, "ENDTIME"):
					d[name] = map[string]interface{}{"TYPE": "INTEGER", "MIN": int64(5), "MAX": int64(1440), "OPERATIONS": int64(3)}
				case strings.Contains(name, "TEMPERATURE"):
					d[name] = map[string]interface{}{"TYPE": "FLOAT", "MIN": 5.0, "MAX": 30.5, "OPERATIONS": int64(3), "UNIT": "°C"}
				default:
					d[name] = map[string]interface{}{"TYPE": "INTEGER", "MIN": int64(0), "MAX": int64(2), "OPERATIONS": int64(3)}
				}
			}
		}
		if addr == "MEQ0000001:4" && key == "VALUES" {
			d["SET_TEMPERATURE"] = map[string]interface{}{"TYPE": "FLOAT", "MIN": 4.5, "MAX": 30.5, "OPERATIONS": int64(7)}
		}
		res = d
	case "getParamset":
		if params[0] == "MEQ0000001" && params[1] == "MASTER" {
			cp := map[string]interface{}{}
			for k, v := range f.master {
				cp[k] = v
			}
			res = cp
		} else {
			res = map[string]interface{}{}
		}
	case "putParamset":
		f.puts++
		for k, v := range params[2].(map[string]interface{}) {
			f.master[k] = v
		}
	case "init":
	}
	w.Header().Set("Content-Type", "text/xml")
	_ = writeResponse(w, res)
}

func wpSetup(t *testing.T, prefixed bool) (*httptest.Server, *fakeThermostat) {
	f := newFakeThermostat(prefixed)
	ccuSrv := httptest.NewServer(f)
	t.Cleanup(ccuSrv.Close)
	cfg := defaultConfig()
	cfg.APIKey = "geheim"
	cfg.RegaURLs = []string{"http://127.0.0.1:1/rega.exe"}
	cfg.Interfaces = []InterfaceConfig{{Name: "BidCos-RF", URLs: []string{ccuSrv.URL}}}
	api := &API{cfg: cfg, ccu: NewCCU(cfg), hub: NewHub(), started: time.Now()}
	srv := httptest.NewServer(api.Middleware(api.Routes()))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestWeekProgramRead(t *testing.T) {
	for _, prefixed := range []bool{true, false} {
		srv, _ := wpSetup(t, prefixed)
		// auch über den Kanal :4 auffindbar
		code, m, _ := do(t, "GET", srv.URL+"/api/devices/MEQ0000001:4/weekprogram", "")
		if code != 200 {
			t.Fatalf("prefixed=%v: %d", prefixed, code)
		}
		if m["address"] != "MEQ0000001" || m["slots"] != float64(13) || m["prefixed"] != prefixed {
			t.Fatalf("Erkennung falsch: %v", m)
		}
		mon := m["program"].(map[string]interface{})["1"].(map[string]interface{})["MONDAY"].([]interface{})
		if len(mon) != 3 || mon[0].(map[string]interface{})["end"] != "06:00" || mon[2].(map[string]interface{})["end"] != "24:00" ||
			mon[1].(map[string]interface{})["temp"] != 21.0 {
			t.Fatalf("Montag falsch: %v", mon)
		}
		if prefixed && (len(m["profiles"].([]interface{})) != 3 || m["activeProfile"] != float64(1)) {
			t.Fatalf("Profile falsch: %v %v", m["profiles"], m["activeProfile"])
		}
		if !prefixed && m["activeProfile"] != nil {
			t.Fatal("ohne Profile darf es kein aktives Profil geben")
		}
	}
}

func TestWeekProgramWrite(t *testing.T) {
	srv, f := wpSetup(t, true)
	body := `{"profile":2,"activeProfile":2,"days":{
		"WORKDAYS":[{"end":"05:30","temp":17},{"end":"07:30","temp":21.4},{"end":"16:00","temp":17},{"end":"22:00","temp":21},{"end":"24:00","temp":17}],
		"wochenende":[{"end":"08:00","temp":17},{"end":"23:00","temp":21.5},{"end":1440,"temp":17}]}}`
	code, m, _ := do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram", body)
	if code != 200 {
		t.Fatalf("Schreiben fehlgeschlagen: %v", m)
	}
	if f.puts != 1 {
		t.Fatalf("es muss genau EIN putParamset sein, waren %d", f.puts)
	}
	g := func(k string) interface{} { f.mu.Lock(); defer f.mu.Unlock(); return f.master[k] }
	checks := map[string]interface{}{
		"P2_ENDTIME_MONDAY_1": int64(330), "P2_TEMPERATURE_MONDAY_2": 21.5, // 21,4 -> 21,5
		"P2_ENDTIME_FRIDAY_5": int64(1440), "P2_ENDTIME_FRIDAY_6": int64(1440), "P2_TEMPERATURE_FRIDAY_13": 17.0,
		"P2_ENDTIME_SUNDAY_2": int64(1380), "P2_TEMPERATURE_SATURDAY_2": 21.5,
		"P1_ENDTIME_MONDAY_1":  int64(360), // anderes Profil unverändert
		"WEEK_PROGRAM_POINTER": int64(1),
	}
	for k, want := range checks {
		if got := g(k); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s = %v, erwartet %v", k, got, want)
		}
	}
	// zurücklesen
	_, m, _ = do(t, "GET", srv.URL+"/api/devices/MEQ0000001/weekprogram", "")
	sat := m["program"].(map[string]interface{})["2"].(map[string]interface{})["SATURDAY"].([]interface{})
	if len(sat) != 3 || m["activeProfile"] != float64(2) {
		t.Fatalf("Zurücklesen falsch: %v / %v", sat, m["activeProfile"])
	}

	// ALL + einzelner Tag überschreibt die Gruppe
	do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram",
		`{"profile":3,"days":{"SUNDAY":[{"end":"24:00","temp":19}],"ALL":[{"end":"24:00","temp":20}]}}`)
	if g("P3_TEMPERATURE_SUNDAY_1") != 19.0 || g("P3_TEMPERATURE_MONDAY_1") != 20.0 {
		t.Fatalf("Gruppe/Einzeltag falsch: %v %v", g("P3_TEMPERATURE_SUNDAY_1"), g("P3_TEMPERATURE_MONDAY_1"))
	}

	// Fehlerfälle – dürfen nichts schreiben
	before := f.puts
	for _, bad := range []string{
		`{"days":{"MONDAY":[{"end":"06:00","temp":17}]}}`,                                                     // endet nicht 24:00
		`{"days":{"MONDAY":[{"end":"06:03","temp":17},{"end":"24:00","temp":17}]}}`,                           // keine 5 min
		`{"days":{"MONDAY":[{"end":"08:00","temp":17},{"end":"07:00","temp":17},{"end":"24:00","temp":17}]}}`, // absteigend
		`{"days":{"MONDAY":[{"end":"24:00","temp":35}]}}`,                                                     // zu warm
		`{"days":{"FUNTAG":[{"end":"24:00","temp":20}]}}`,                                                     // unbekannter Tag
		`{"profile":7,"days":{"MONDAY":[{"end":"24:00","temp":20}]}}`,                                         // kein Profil 7
		`{"days":{"MONDAY":[]}}`,
	} {
		if code, _, _ := do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram", bad); code != 400 {
			t.Errorf("sollte 400 sein: %s (war %d)", bad, code)
		}
	}
	if f.puts != before {
		t.Fatal("ungültiger Plan wurde trotzdem geschrieben")
	}
	// 14 Abschnitte geht nicht
	var sb strings.Builder
	for i := 1; i <= 14; i++ {
		fmt.Fprintf(&sb, `{"end":"%s","temp":20},`, fmtMinutes(i*60))
	}
	tooMany := `{"days":{"MONDAY":[` + strings.TrimSuffix(sb.String(), ",") + `]}}`
	if code, _, _ := do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram", tooMany); code != 400 {
		t.Fatal("zu viele Abschnitte nicht erkannt")
	}
}

func TestWeekProgramUnprefixed(t *testing.T) {
	srv, f := wpSetup(t, false)
	code, m, _ := do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram",
		`{"days":{"TUESDAY":[{"end":"06:30","temp":18},{"end":"24:00","temp":20}]}}`)
	if code != 200 || f.master["ENDTIME_TUESDAY_1"] != int64(390) || f.master["TEMPERATURE_TUESDAY_2"] != 20.0 {
		t.Fatalf("ohne Präfix falsch: %v %v", m, f.master["ENDTIME_TUESDAY_1"])
	}
	before := f.puts
	if code, _, _ := do(t, "PUT", srv.URL+"/api/devices/MEQ0000001/weekprogram",
		`{"activeProfile":2,"days":{"TUESDAY":[{"end":"24:00","temp":20}]}}`); code != 400 || f.puts != before {
		t.Fatal("Profilumschaltung ohne Profile müsste abgelehnt werden, ohne zu schreiben")
	}
}
