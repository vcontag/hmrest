package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type API struct {
	cfg     *Config
	cfgPath string
	keyMu   sync.RWMutex
	ccu     *CCU
	hub     *Hub
	cb      *Callback
	started time.Time
	// Prüfung der HomeMatic-WebUI-Sitzung (für die Key-Verwaltung)
	sessions *SessionChecker
}

func (a *API) Routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", a.ui)
	m.HandleFunc("GET /setup/status", a.setupStatus)
	m.HandleFunc("POST /setup/apikey", a.setupKey)
	m.HandleFunc("GET /setup/apikey", a.getKey)
	m.HandleFunc("GET /api", a.index)
	m.HandleFunc("GET /api/info", a.info)
	m.HandleFunc("GET /api/interfaces", a.interfaces)

	m.HandleFunc("GET /api/devices", a.listDevices)
	m.HandleFunc("GET /api/devices/{address}", a.getDevice)
	m.HandleFunc("GET /api/devices/{address}/values", a.getValues)
	m.HandleFunc("PUT /api/devices/{address}/values", a.putValues)
	m.HandleFunc("GET /api/devices/{address}/values/{param}", a.getValue)
	m.HandleFunc("PUT /api/devices/{address}/values/{param}", a.setValue)
	m.HandleFunc("POST /api/devices/{address}/values/{param}", a.setValue)
	m.HandleFunc("GET /api/devices/{address}/paramsets/{key}", a.getParamset)
	m.HandleFunc("PUT /api/devices/{address}/paramsets/{key}", a.putParamset)
	m.HandleFunc("GET /api/devices/{address}/paramsets/{key}/description", a.getParamsetDesc)

	m.HandleFunc("GET /api/devices/{address}/weekprogram", a.getWeekProgram)
	m.HandleFunc("PUT /api/devices/{address}/weekprogram", a.putWeekProgram)

	m.HandleFunc("GET /api/rooms", a.rooms)
	m.HandleFunc("GET /api/functions", a.functions)

	m.HandleFunc("GET /api/sysvars", a.sysvars)
	m.HandleFunc("GET /api/sysvars/{ref}", a.sysvar)
	m.HandleFunc("PUT /api/sysvars/{ref}", a.setSysvar)
	m.HandleFunc("POST /api/sysvars/{ref}", a.setSysvar)

	m.HandleFunc("GET /api/programs", a.programs)
	m.HandleFunc("GET /api/programs/{ref}", a.program)
	m.HandleFunc("POST /api/programs/{ref}/run", a.runProgram)
	m.HandleFunc("PUT /api/programs/{ref}", a.updateProgram)

	m.HandleFunc("POST /api/rega", a.rega)
	m.HandleFunc("GET /api/events", a.events)
	m.HandleFunc("GET /api/events/last", a.eventsLast)
	return m
}

// ---------- Middleware ----------

func (a *API) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.CORS != "" {
			w.Header().Set("Access-Control-Allow-Origin", a.cfg.CORS)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api") {
			if !a.cfg.NoAuth && a.apiKey() == "" {
				writeJSON(w, 401, map[string]interface{}{"error": "Noch kein API-Key – bitte auf der Startseite erzeugen", "setup": true})
				return
			}
			if !a.cfg.NoAuth && !a.authorized(r) {
				writeJSON(w, 401, map[string]string{"error": "API-Key fehlt oder ist falsch"})
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if a.cfg.ReadOnly {
					writeJSON(w, 403, map[string]string{"error": "Add-on läuft im Nur-Lesen-Modus"})
					return
				}
				log.Printf("%s %s von %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func providedKey(r *http.Request) string {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			key = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if key == "" {
		key = r.URL.Query().Get("apikey")
	}
	return strings.TrimSpace(key)
}

func (a *API) authorized(r *http.Request) bool {
	key := providedKey(r)
	cur := a.apiKey()
	return key != "" && cur != "" && subtle.ConstantTimeCompare([]byte(key), []byte(cur)) == 1
}

func (a *API) apiKey() string {
	a.keyMu.RLock()
	defer a.keyMu.RUnlock()
	return a.cfg.APIKey
}

// ---------- Einrichtung: API-Key per Button ----------

func (a *API) setupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"keySet": a.apiKey() != "", "noAuth": a.cfg.NoAuth, "version": version})
}

// providedSID liest die WebUI-Sitzung aus ?sid= oder dem Header X-HM-Session.
func providedSID(r *http.Request) string {
	if s := r.Header.Get("X-HM-Session"); s != "" {
		return s
	}
	return r.URL.Query().Get("sid")
}

func (a *API) sessionOK(r *http.Request) bool {
	sid := providedSID(r)
	if sid == "" || a.sessions == nil {
		return false
	}
	ok := a.sessions.Valid(sid)
	if !ok {
		log.Printf("Ungültige WebUI-Sitzung von %s", r.RemoteAddr)
	}
	return ok
}

// newKeyLocked erzeugt und speichert einen neuen Key (keyMu muss gesperrt sein).
func (a *API) newKeyLocked(r *http.Request) error {
	old := a.cfg.APIKey
	a.cfg.APIKey = randomKey()
	if err := saveConfig(a.cfgPath, a.cfg); err != nil {
		a.cfg.APIKey = old
		return httpErr(500, "Konfiguration konnte nicht gespeichert werden: %v", err)
	}
	log.Printf("Neuer API-Key erzeugt (von %s)", r.RemoteAddr)
	return nil
}

// getKey liefert den aktuellen API-Key – nur mit gültiger WebUI-Sitzung.
// Gibt es noch keinen, wird er dabei erzeugt (Ersteinrichtung).
func (a *API) getKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.cfg.NoAuth {
		writeErr(w, httpErr(400, "Authentifizierung ist abgeschaltet (noAuth)"))
		return
	}
	if !a.sessionOK(r) {
		writeJSON(w, 401, map[string]interface{}{"error": "Keine gültige HomeMatic-Sitzung – bitte über die CCU-WebUI öffnen", "session": true})
		return
	}
	a.keyMu.Lock()
	defer a.keyMu.Unlock()
	if a.cfg.APIKey == "" {
		if err := a.newKeyLocked(r); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]string{"apiKey": a.cfg.APIKey})
}

// setupKey erzeugt einen neuen API-Key. Erlaubt mit gültiger WebUI-Sitzung
// oder mit dem aktuellen API-Key – anonym nicht mehr.
func (a *API) setupKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.cfg.NoAuth {
		writeErr(w, httpErr(400, "Authentifizierung ist abgeschaltet (noAuth)"))
		return
	}
	keyOK := a.authorized(r)
	if !keyOK && !a.sessionOK(r) {
		writeJSON(w, 401, map[string]interface{}{"error": "Nur mit gültiger HomeMatic-Sitzung oder dem aktuellen API-Key", "session": true})
		return
	}
	a.keyMu.Lock()
	defer a.keyMu.Unlock()
	if err := a.newKeyLocked(r); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"apiKey": a.cfg.APIKey})
}

// ---------- Helfer ----------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var he *HTTPError
	var f *Fault
	switch {
	case errors.As(err, &he):
		writeJSON(w, he.Status, map[string]string{"error": he.Msg})
	case errors.As(err, &f):
		writeJSON(w, 502, map[string]interface{}{"error": f.String, "faultCode": f.Code})
	default:
		writeJSON(w, 502, map[string]string{"error": err.Error()})
	}
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	return []byte(strings.TrimSpace(string(b))), err
}

// readValue akzeptiert {"value": x}, einen nackten JSON-Wert, Text oder ?value=.
func readValue(r *http.Request) (interface{}, error) {
	if q, ok := r.URL.Query()["value"]; ok && len(q) > 0 {
		return q[0], nil
	}
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, httpErr(400, "kein Wert übergeben (Body {\"value\": ...} oder ?value=)")
	}
	var v interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body), nil
	}
	if m, ok := v.(map[string]interface{}); ok {
		if x, ok := m["value"]; ok {
			return x, nil
		}
		return nil, httpErr(400, "Feld \"value\" fehlt")
	}
	return v, nil
}

func readObject(r *http.Request) (map[string]interface{}, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, httpErr(400, "JSON-Objekt erwartet: %v", err)
	}
	return m, nil
}

func qbool(r *http.Request, k string) bool {
	v := strings.ToLower(r.URL.Query().Get(k))
	return v == "1" || v == "true" || v == "yes"
}

// ---------- UI / Info ----------

func (a *API) ui(w http.ResponseWriter, r *http.Request) {
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (a *API) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"name": "HM-REST", "version": version,
		"endpoints": []string{
			"GET  /api/info", "GET  /api/interfaces",
			"GET  /api/devices?interface=&values=1|full&paramsets=MASTER&raw=1",
			"GET  /api/devices/{address}?values=1|full",
			"GET  /api/devices/{address}/values", "PUT  /api/devices/{address}/values  {\"LEVEL\":0.5,...}",
			"GET  /api/devices/{address}/values/{param}", "PUT|POST /api/devices/{address}/values/{param}  {\"value\":...}",
			"GET  /api/devices/{address}/paramsets/{VALUES|MASTER|peer}[?full=1]",
			"PUT  /api/devices/{address}/paramsets/{key}  {...}",
			"GET  /api/devices/{address}/paramsets/{key}/description",
			"GET|PUT /api/devices/{address}/weekprogram  {\"profile\":1,\"days\":{\"WORKDAYS\":[{\"end\":\"06:00\",\"temp\":17},{\"end\":\"24:00\",\"temp\":21}]}}",
			"GET  /api/rooms", "GET  /api/functions",
			"GET  /api/sysvars", "GET|PUT /api/sysvars/{id|name}",
			"GET  /api/programs", "POST /api/programs/{id|name}/run", "PUT /api/programs/{id|name} {\"active\":true}",
			"POST /api/rega (nur mit allowScript)", "GET  /api/events (SSE)", "GET  /api/events/last",
		},
	})
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	ifs := []interface{}{}
	idx := a.ccu.Index(false)
	counts := map[string]int{}
	for _, e := range idx {
		if !isChannel(e) {
			counts[e.Iface]++
		}
	}
	for _, i := range a.ccu.Interfaces() {
		s := i.Status()
		s["devices"] = counts[i.Name]
		ifs = append(ifs, s)
	}
	out := map[string]interface{}{
		"version": version, "uptime": time.Since(a.started).Round(time.Second).String(),
		"interfaces": ifs, "rega": a.ccu.Rega.URL(), "readOnly": a.cfg.ReadOnly,
		"sseClients": a.hub.Count(),
	}
	if a.cb != nil {
		out["events"] = a.cb.Status()
	}
	writeJSON(w, 200, out)
}

func (a *API) interfaces(w http.ResponseWriter, r *http.Request) {
	out := []interface{}{}
	for _, i := range a.ccu.Interfaces() {
		_, _ = i.client0()
		out = append(out, i.Status())
	}
	writeJSON(w, 200, out)
}

// ---------- Geräte ----------

type valueJob struct {
	ch  map[string]interface{}
	key string
}

func (a *API) fillValues(jobs []valueJob, mode string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, j := range jobs {
		wg.Add(1)
		go func(j valueJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			addr := fmt.Sprint(j.ch["address"])
			var v interface{}
			var err error
			if mode == "full" {
				v, err = a.ccu.ValuesMeta(addr, j.key)
			} else {
				v, err = a.ccu.GetParamset(addr, j.key)
			}
			mu.Lock()
			defer mu.Unlock()
			field := strings.ToLower(j.key)
			if err != nil {
				errs, _ := j.ch["errors"].(map[string]string)
				if errs == nil {
					errs = map[string]string{}
					j.ch["errors"] = errs
				}
				errs[j.key] = err.Error()
				return
			}
			j.ch[field] = v
		}(j)
	}
	wg.Wait()
}

func valueMode(r *http.Request) string {
	switch strings.ToLower(r.URL.Query().Get("values")) {
	case "full", "meta":
		return "full"
	case "1", "true", "yes", "plain":
		return "plain"
	}
	return ""
}

func paramsetsWanted(r *http.Request, mode string) []string {
	var keys []string
	if mode != "" {
		keys = append(keys, "VALUES")
	}
	for _, k := range strings.Split(r.URL.Query().Get("paramsets"), ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "" && k != "VALUES" {
			keys = append(keys, k)
		} else if k == "VALUES" && mode == "" {
			keys = append(keys, k)
		}
	}
	return keys
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	idx := a.ccu.Index(qbool(r, "refresh"))
	meta, _ := a.ccu.Rega.Meta(qbool(r, "refresh"))
	ifFilter := r.URL.Query().Get("interface")
	room := r.URL.Query().Get("room")
	raw := qbool(r, "raw")
	mode := valueMode(r)
	keys := paramsetsWanted(r, mode)
	if mode == "" && len(keys) > 0 {
		mode = "plain"
	}

	devs := map[string]map[string]interface{}{}
	var chans []*DevEntry
	for _, e := range idx {
		if ifFilter != "" && !strings.EqualFold(e.Iface, ifFilter) {
			continue
		}
		if isChannel(e) {
			chans = append(chans, e)
			continue
		}
		d := entryOut(e, meta, raw)
		d["channels"] = []map[string]interface{}{}
		devs[strings.ToUpper(fmt.Sprint(e.Desc["ADDRESS"]))] = d
	}
	var jobs []valueJob
	for _, e := range chans {
		d := devs[strings.ToUpper(fmt.Sprint(e.Desc["PARENT"]))]
		if d == nil {
			continue
		}
		c := entryOut(e, meta, raw)
		if room != "" {
			ok := false
			for _, rn := range meta.Rooms[strings.ToUpper(fmt.Sprint(c["address"]))] {
				if strings.EqualFold(rn, room) {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		d["channels"] = append(d["channels"].([]map[string]interface{}), c)
		for _, k := range keys {
			if hasParamset(e, k) {
				jobs = append(jobs, valueJob{c, k})
			}
		}
	}
	a.fillValues(jobs, mode)

	list := make([]map[string]interface{}, 0, len(devs))
	for _, d := range devs {
		ch := d["channels"].([]map[string]interface{})
		if room != "" && len(ch) == 0 {
			continue
		}
		sort.Slice(ch, func(i, j int) bool { return toInt64(ch[i]["index"]) < toInt64(ch[j]["index"]) })
		list = append(list, d)
	}
	sortByName(list)
	writeJSON(w, 200, list)
}

func (a *API) getDevice(w http.ResponseWriter, r *http.Request) {
	e, _, err := a.ccu.Lookup(r.PathValue("address"))
	if err != nil {
		writeErr(w, err)
		return
	}
	meta, _ := a.ccu.Rega.Meta(qbool(r, "refresh"))
	raw := qbool(r, "raw")
	mode := valueMode(r)
	keys := paramsetsWanted(r, mode)
	if mode == "" && len(keys) > 0 {
		mode = "plain"
	}
	out := entryOut(e, meta, raw)
	var jobs []valueJob
	if isChannel(e) {
		for _, k := range keys {
			if hasParamset(e, k) {
				jobs = append(jobs, valueJob{out, k})
			}
		}
		if p, _, err := a.ccu.Lookup(fmt.Sprint(e.Desc["PARENT"])); err == nil {
			if n, ok := meta.Names[strings.ToUpper(fmt.Sprint(p.Desc["ADDRESS"]))]; ok {
				out["parentName"] = n
			}
		}
	} else {
		var chans []map[string]interface{}
		children, _ := e.Desc["CHILDREN"].([]interface{})
		for _, c := range children {
			ce, _, err := a.ccu.Lookup(fmt.Sprint(c))
			if err != nil {
				continue
			}
			co := entryOut(ce, meta, raw)
			chans = append(chans, co)
			for _, k := range keys {
				if hasParamset(ce, k) {
					jobs = append(jobs, valueJob{co, k})
				}
			}
		}
		for _, k := range keys { // Geräte-MASTER (z.B. Gerätekonfiguration)
			if hasParamset(e, k) {
				jobs = append(jobs, valueJob{out, k})
			}
		}
		sort.Slice(chans, func(i, j int) bool { return toInt64(chans[i]["index"]) < toInt64(chans[j]["index"]) })
		out["channels"] = chans
	}
	a.fillValues(jobs, mode)
	writeJSON(w, 200, out)
}

func (a *API) getValues(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("address")
	var v interface{}
	var err error
	if qbool(r, "plain") {
		v, err = a.ccu.GetParamset(addr, "VALUES")
	} else {
		v, err = a.ccu.ValuesMeta(addr, "VALUES")
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) putValues(w http.ResponseWriter, r *http.Request) {
	m, err := readObject(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	set, err := a.ccu.PutParamset(r.PathValue("address"), "VALUES", m)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "set": set})
}

func (a *API) getValue(w http.ResponseWriter, r *http.Request) {
	addr, param := r.PathValue("address"), r.PathValue("param")
	desc, err := a.ccu.ParamsetDescription(addr, "VALUES")
	if err != nil {
		writeErr(w, err)
		return
	}
	pd, ok := desc[param].(map[string]interface{})
	if !ok {
		writeErr(w, httpErr(404, "Parameter %s existiert nicht auf %s", param, addr))
		return
	}
	m := paramMeta(pd, nil, param)
	if toInt64(pd["OPERATIONS"])&1 != 0 {
		v, err := a.ccu.GetValue(addr, param)
		if err != nil {
			writeErr(w, err)
			return
		}
		m["value"] = v
	}
	m["address"], m["param"] = strings.ToUpper(addr), param
	writeJSON(w, 200, m)
}

func (a *API) setValue(w http.ResponseWriter, r *http.Request) {
	v, err := readValue(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	sent, err := a.ccu.SetValue(r.PathValue("address"), r.PathValue("param"), v)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "address": strings.ToUpper(r.PathValue("address")), "param": r.PathValue("param"), "value": sent})
}

func (a *API) getParamset(w http.ResponseWriter, r *http.Request) {
	addr, key := r.PathValue("address"), r.PathValue("key")
	var v interface{}
	var err error
	if qbool(r, "full") {
		v, err = a.ccu.ValuesMeta(addr, key)
	} else {
		v, err = a.ccu.GetParamset(addr, key)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) getParamsetDesc(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.ParamsetDescription(r.PathValue("address"), descKeyFor(r.PathValue("key")))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) putParamset(w http.ResponseWriter, r *http.Request) {
	m, err := readObject(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	set, err := a.ccu.PutParamset(r.PathValue("address"), r.PathValue("key"), m)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "set": set})
}

// ---------- Räume / Gewerke ----------

func (a *API) groupList(w http.ResponseWriter, r *http.Request, funcs bool) {
	meta, err := a.ccu.Rega.Meta(qbool(r, "refresh"))
	if err != nil && len(meta.Names) == 0 {
		writeErr(w, err)
		return
	}
	src := meta.RoomList
	if funcs {
		src = meta.FuncList
	}
	idx := a.ccu.Index(false)
	out := []map[string]interface{}{}
	for name, addrs := range src {
		chans := []map[string]interface{}{}
		for _, ad := range addrs {
			c := map[string]interface{}{"address": ad, "name": meta.Names[ad]}
			if e := idx[ad]; e != nil {
				c["type"] = e.Desc["TYPE"]
				c["interface"] = e.Iface
				if p, ok := e.Desc["PARENT"]; ok {
					c["deviceName"] = meta.Names[strings.ToUpper(fmt.Sprint(p))]
				}
			}
			chans = append(chans, c)
		}
		out = append(out, map[string]interface{}{"name": name, "channels": chans})
	}
	sortByName(out)
	writeJSON(w, 200, out)
}

func (a *API) rooms(w http.ResponseWriter, r *http.Request)     { a.groupList(w, r, false) }
func (a *API) functions(w http.ResponseWriter, r *http.Request) { a.groupList(w, r, true) }

// ---------- Systemvariablen / Programme ----------

func (a *API) sysvars(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.Rega.SysVars()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) sysvar(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.Rega.FindSysVar(r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) setSysvar(w http.ResponseWriter, r *http.Request) {
	val, err := readValue(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := a.ccu.Rega.SetSysVar(r.PathValue("ref"), val)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) programs(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.Rega.Programs()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) program(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.Rega.FindProgram(r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) runProgram(w http.ResponseWriter, r *http.Request) {
	v, err := a.ccu.Rega.RunProgram(r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "program": v})
}

func (a *API) updateProgram(w http.ResponseWriter, r *http.Request) {
	m, err := readObject(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	act, ok := m["active"]
	if !ok {
		writeErr(w, httpErr(400, "Feld \"active\" fehlt"))
		return
	}
	b, err := toBool(act)
	if err != nil {
		writeErr(w, httpErr(400, "%v", err))
		return
	}
	v, err := a.ccu.Rega.SetProgramActive(r.PathValue("ref"), b)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) rega(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.AllowScript {
		writeErr(w, httpErr(403, "Skriptausführung ist deaktiviert (allowScript in config.json)"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := a.ccu.Rega.Run(string(body))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"output": out})
}

// ---------- Events (SSE) ----------

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, httpErr(500, "Streaming nicht unterstützt"))
		return
	}
	if a.cb == nil {
		writeErr(w, httpErr(503, "Events sind in der Konfiguration deaktiviert"))
		return
	}
	addrF := strings.ToUpper(r.URL.Query().Get("address"))
	paramF := map[string]bool{}
	for _, p := range strings.Split(r.URL.Query().Get("param"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			paramF[p] = true
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, ": verbunden\n\n")
	fl.Flush()

	ch := a.hub.Subscribe()
	defer a.hub.Unsubscribe(ch)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if addrF != "" && !strings.HasPrefix(strings.ToUpper(e.Address), addrF) {
				continue
			}
			if len(paramF) > 0 && !paramF[e.Param] {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

func (a *API) eventsLast(w http.ResponseWriter, r *http.Request) {
	list := a.hub.Last()
	addrF := strings.ToUpper(r.URL.Query().Get("address"))
	out := []Event{}
	for _, e := range list {
		if addrF == "" || strings.HasPrefix(strings.ToUpper(e.Address), addrF) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	writeJSON(w, 200, out)
}
