package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type InterfaceConfig struct {
	Name    string   `json:"name"`
	URLs    []string `json:"urls"`
	Enabled *bool    `json:"enabled,omitempty"`
}

var ErrNotFound = errors.New("nicht gefunden")

type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

func httpErr(status int, format string, a ...interface{}) error {
	return &HTTPError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// ---------- Schnittstelle (BidCos-RF, HmIP-RF, ...) ----------

type Iface struct {
	Name      string
	cfg       InterfaceConfig
	mu        sync.Mutex
	client    RPCClient
	lastProbe time.Time
	lastError string
	sem       chan struct{}
}

// RPCClient ist XML-RPC (http://…) oder BIN-RPC (bin://…, xmlrpc_bin://…).
type RPCClient interface {
	Call(method string, params ...interface{}) (interface{}, error)
	Endpoint() string
}

func isBinURL(u string) bool {
	return strings.HasPrefix(u, "bin://") || strings.HasPrefix(u, "xmlrpc_bin://") || strings.HasPrefix(u, "binrpc://")
}

// binURLFor macht aus http://host:port[/pfad] die BIN-RPC-Adresse bin://host:port.
func binURLFor(u string) string {
	for _, p := range []string{"http://", "https://"} {
		if strings.HasPrefix(u, p) {
			hp := strings.TrimPrefix(u, p)
			if i := strings.IndexByte(hp, '/'); i >= 0 {
				hp = hp[:i]
			}
			if hp != "" {
				return "bin://" + hp
			}
		}
	}
	return ""
}

func newRPCClient(url string, timeout time.Duration) RPCClient {
	if isBinURL(url) {
		return NewBinRPCClient(url, timeout)
	}
	return NewXMLRPCClient(url, timeout)
}

// IsBin meldet, ob die Schnittstelle BIN-RPC spricht (bestimmt die Callback-Art).
// Maßgeblich ist das tatsächlich erkannte Protokoll, sonst die Konfiguration.
func (i *Iface) IsBin() bool {
	i.mu.Lock()
	c := i.client
	i.mu.Unlock()
	if c != nil {
		_, ok := c.(*BinRPCClient)
		return ok
	}
	for _, u := range i.cfg.URLs {
		if isBinURL(u) {
			return true
		}
	}
	return false
}

func (i *Iface) client0() (RPCClient, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.client != nil {
		return i.client, nil
	}
	if !i.lastProbe.IsZero() && time.Since(i.lastProbe) < 30*time.Second {
		return nil, fmt.Errorf("Schnittstelle %s nicht erreichbar: %s", i.Name, i.lastError)
	}
	i.lastProbe = time.Now()
	for _, u := range i.cfg.URLs {
		var err error
		if isBinURL(u) {
			// BIN-RPC (z.B. CUxD): erreichbar, wenn der Port Verbindungen annimmt
			var c net.Conn
			if c, err = net.DialTimeout("tcp", NewBinRPCClient(u, 0).addr, 3*time.Second); err == nil {
				c.Close()
			}
		} else {
			_, err = newRPCClient(u, 5*time.Second).Call("system.listMethods")
		}
		if err == nil || isFault(err) { // jede gültige RPC-Antwort reicht
			i.client = newRPCClient(u, 60*time.Second)
			i.lastError = ""
			return i.client, nil
		}
		// http:// eingetragen, aber der Dienst spricht BIN-RPC (z.B. CUxD auf 8701)?
		if !isBinURL(u) {
			if bu := binURLFor(u); bu != "" {
				_, berr := NewBinRPCClient(bu, 5*time.Second).Call("system.listMethods")
				if berr == nil || isFault(berr) {
					log.Printf("Schnittstelle %s spricht BIN-RPC – nutze %s statt %s", i.Name, bu, u)
					i.client = NewBinRPCClient(bu, 60*time.Second)
					i.lastError = ""
					return i.client, nil
				}
			}
		}
		i.lastError = err.Error()
	}
	return nil, fmt.Errorf("Schnittstelle %s nicht erreichbar: %s", i.Name, i.lastError)
}

func (i *Iface) Call(method string, params ...interface{}) (interface{}, error) {
	c, err := i.client0()
	if err != nil {
		return nil, httpErr(503, "%s", err.Error())
	}
	i.sem <- struct{}{}
	defer func() { <-i.sem }()
	v, err := c.Call(method, params...)
	if err != nil && !isFault(err) {
		i.mu.Lock()
		i.client = nil
		i.lastError = err.Error()
		i.mu.Unlock()
	}
	return v, err
}

func (i *Iface) Status() map[string]interface{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	url := ""
	if i.client != nil {
		url = i.client.Endpoint()
	}
	return map[string]interface{}{
		"name": i.Name, "url": url, "available": i.client != nil, "error": i.lastError,
	}
}

// ---------- CCU ----------

type DevEntry struct {
	Iface string
	Desc  map[string]interface{}
}

type descEntry struct {
	d  map[string]interface{}
	at time.Time
}

type CCU struct {
	ifaces []*Iface
	byName map[string]*Iface
	Rega   *Rega

	mu         sync.RWMutex
	index      map[string]*DevEntry
	indexAt    time.Time
	indexDirty bool

	dmu   sync.Mutex
	descs map[string]descEntry
}

func NewCCU(cfg *Config) *CCU {
	c := &CCU{byName: map[string]*Iface{}, descs: map[string]descEntry{}, Rega: NewRega(cfg.RegaURLs)}
	for _, ic := range cfg.Interfaces {
		if ic.Enabled != nil && !*ic.Enabled {
			continue
		}
		i := &Iface{Name: ic.Name, cfg: ic, sem: make(chan struct{}, 3)}
		c.ifaces = append(c.ifaces, i)
		c.byName[ic.Name] = i
	}
	return c
}

func (c *CCU) Interfaces() []*Iface { return c.ifaces }

func (c *CCU) InvalidateIndex() {
	c.mu.Lock()
	c.indexDirty = true
	c.mu.Unlock()
	c.dmu.Lock()
	c.descs = map[string]descEntry{}
	c.dmu.Unlock()
}

func (c *CCU) refreshIndex() map[string]*DevEntry {
	idx := map[string]*DevEntry{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, i := range c.ifaces {
		wg.Add(1)
		go func(i *Iface) {
			defer wg.Done()
			v, err := i.Call("listDevices")
			if err != nil {
				return
			}
			arr, _ := v.([]interface{})
			mu.Lock()
			defer mu.Unlock()
			for _, e := range arr {
				m, ok := e.(map[string]interface{})
				if !ok {
					continue
				}
				addr := strings.ToUpper(fmt.Sprint(m["ADDRESS"]))
				idx[addr] = &DevEntry{Iface: i.Name, Desc: m}
			}
		}(i)
	}
	wg.Wait()
	c.mu.Lock()
	c.index, c.indexAt, c.indexDirty = idx, time.Now(), false
	c.mu.Unlock()
	return idx
}

// Index liefert alle Geräte und Kanäle (Adresse -> Beschreibung).
func (c *CCU) Index(force bool) map[string]*DevEntry {
	c.mu.RLock()
	idx, at, dirty := c.index, c.indexAt, c.indexDirty
	c.mu.RUnlock()
	if idx == nil || force || dirty || time.Since(at) > 10*time.Minute || (len(idx) == 0 && time.Since(at) > 15*time.Second) {
		return c.refreshIndex()
	}
	return idx
}

func (c *CCU) Lookup(addr string) (*DevEntry, *Iface, error) {
	addr = strings.ToUpper(strings.TrimSpace(addr))
	e := c.Index(false)[addr]
	if e == nil {
		c.mu.RLock()
		age := time.Since(c.indexAt)
		c.mu.RUnlock()
		if age > 10*time.Second {
			e = c.Index(true)[addr]
		}
	}
	if e == nil {
		return nil, nil, httpErr(404, "Adresse %s nicht gefunden", addr)
	}
	return e, c.byName[e.Iface], nil
}

func (c *CCU) ParamsetDescription(addr, key string) (map[string]interface{}, error) {
	e, i, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	addr = fmt.Sprint(e.Desc["ADDRESS"])
	ck := e.Iface + "|" + addr + "|" + key
	c.dmu.Lock()
	if d, ok := c.descs[ck]; ok && time.Since(d.at) < time.Hour {
		c.dmu.Unlock()
		return d.d, nil
	}
	c.dmu.Unlock()
	v, err := i.Call("getParamsetDescription", addr, key)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]interface{})
	if m == nil {
		m = map[string]interface{}{}
	}
	c.dmu.Lock()
	c.descs[ck] = descEntry{d: m, at: time.Now()}
	c.dmu.Unlock()
	return m, nil
}

func (c *CCU) GetParamset(addr, key string) (map[string]interface{}, error) {
	e, i, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	v, err := i.Call("getParamset", fmt.Sprint(e.Desc["ADDRESS"]), key)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]interface{})
	if m == nil {
		m = map[string]interface{}{}
	}
	if desc, err := c.ParamsetDescription(addr, descKeyFor(key)); err == nil {
		m = normalizeSet(addr, desc, m)
	}
	return m, nil
}

func (c *CCU) GetValue(addr, param string) (interface{}, error) {
	e, i, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	v, err := i.Call("getValue", fmt.Sprint(e.Desc["ADDRESS"]), param)
	if err != nil {
		return nil, err
	}
	if desc, derr := c.ParamsetDescription(addr, "VALUES"); derr == nil {
		if pd, ok := desc[param].(map[string]interface{}); ok {
			nv, ok := normValue(pd, v)
			if !ok {
				warnBad(addr, param, v)
				return nil, nil
			}
			return nv, nil
		}
	}
	return v, nil
}

func descKeyFor(key string) string {
	k := strings.ToUpper(key)
	if k == "VALUES" || k == "MASTER" {
		return k
	}
	return "LINK" // Peer-Adresse = Link-Paramset
}

func (c *CCU) SetValue(addr, param string, raw interface{}) (interface{}, error) {
	e, i, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	desc, err := c.ParamsetDescription(addr, "VALUES")
	if err != nil {
		return nil, err
	}
	pd, ok := desc[param].(map[string]interface{})
	if !ok {
		return nil, httpErr(404, "Parameter %s existiert nicht auf %s", param, addr)
	}
	if toInt64(pd["OPERATIONS"])&2 == 0 {
		return nil, httpErr(403, "Parameter %s ist nicht schreibbar", param)
	}
	v, err := convertValue(param, pd, raw)
	if err != nil {
		return nil, httpErr(400, "%s", err.Error())
	}
	if _, err := i.Call("setValue", fmt.Sprint(e.Desc["ADDRESS"]), param, v); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *CCU) PutParamset(addr, key string, values map[string]interface{}) (map[string]interface{}, error) {
	e, i, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	desc, err := c.ParamsetDescription(addr, descKeyFor(key))
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for name, raw := range values {
		pd, ok := desc[name].(map[string]interface{})
		if !ok {
			return nil, httpErr(404, "Parameter %s existiert nicht im Paramset %s", name, key)
		}
		if toInt64(pd["OPERATIONS"])&2 == 0 {
			return nil, httpErr(403, "Parameter %s ist nicht schreibbar", name)
		}
		v, err := convertValue(name, pd, raw)
		if err != nil {
			return nil, httpErr(400, "%s", err.Error())
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil, httpErr(400, "keine Werte übergeben")
	}
	if _, err := i.Call("putParamset", fmt.Sprint(e.Desc["ADDRESS"]), key, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ValuesMeta kombiniert Werte und Beschreibung eines Kanals.
func (c *CCU) ValuesMeta(addr string, key string) (map[string]interface{}, error) {
	desc, err := c.ParamsetDescription(addr, descKeyFor(key))
	if err != nil {
		return nil, err
	}
	vals, err := c.GetParamset(addr, key)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	for name, d := range desc {
		dm, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		out[name] = paramMeta(dm, vals, name)
	}
	return out, nil
}

func paramMeta(dm map[string]interface{}, vals map[string]interface{}, name string) map[string]interface{} {
	ops := toInt64(dm["OPERATIONS"])
	m := map[string]interface{}{
		"type":     dm["TYPE"],
		"unit":     dm["UNIT"],
		"min":      dm["MIN"],
		"max":      dm["MAX"],
		"default":  dm["DEFAULT"],
		"flags":    toInt64(dm["FLAGS"]),
		"readable": ops&1 != 0,
		"writable": ops&2 != 0,
		"event":    ops&4 != 0,
	}
	if vl, ok := dm["VALUE_LIST"]; ok {
		m["valueList"] = vl
	}
	if sp, ok := dm["SPECIAL"]; ok {
		m["special"] = sp
	}
	if vals != nil {
		if v, ok := vals[name]; ok {
			m["value"] = v
		}
	}
	return m
}

// convertValue wandelt JSON-Eingaben in den Typ, den die CCU erwartet.
func convertValue(name string, pd map[string]interface{}, raw interface{}) (interface{}, error) {
	t, _ := pd["TYPE"].(string)
	checkRange := func(f float64) error {
		min, e1 := toFloat(pd["MIN"])
		max, e2 := toFloat(pd["MAX"])
		if e1 == nil && e2 == nil && min < max && (f < min-1e-9 || f > max+1e-9) {
			// Sonderwerte (SPECIAL) zulassen
			if sp, ok := pd["SPECIAL"].([]interface{}); ok {
				for _, s := range sp {
					if sm, ok := s.(map[string]interface{}); ok {
						if sv, err := toFloat(sm["VALUE"]); err == nil && sv == f {
							return nil
						}
					}
				}
			}
			return fmt.Errorf("%s: Wert %v außerhalb %v..%v", name, f, min, max)
		}
		return nil
	}
	switch t {
	case "FLOAT":
		f, err := toFloat(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		return f, checkRange(f)
	case "INTEGER":
		f, err := toFloat(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		n := int(math.Round(f))
		return n, checkRange(float64(n))
	case "ENUM":
		if s, ok := raw.(string); ok {
			if _, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
				offset := int(toInt64(pd["MIN"]))
				if vl, ok := pd["VALUE_LIST"].([]interface{}); ok {
					for idx, e := range vl {
						if strings.EqualFold(fmt.Sprint(e), strings.TrimSpace(s)) {
							return offset + idx, nil
						}
					}
				}
				return nil, fmt.Errorf("%s: %q nicht in Werteliste %v", name, s, pd["VALUE_LIST"])
			}
		}
		f, err := toFloat(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		n := int(math.Round(f))
		return n, checkRange(float64(n))
	case "BOOL", "ACTION":
		b, err := toBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		return b, nil
	case "STRING":
		if raw == nil {
			return "", nil
		}
		if s, ok := raw.(string); ok {
			return s, nil
		}
		return fmt.Sprint(raw), nil
	}
	return raw, nil
}

// ---------- Ausgabe-Helfer ----------

var descFields = []struct{ key, out string }{
	{"TYPE", "type"}, {"FIRMWARE", "firmware"}, {"AVAILABLE_FIRMWARE", "availableFirmware"},
	{"PARAMSETS", "paramsets"}, {"DIRECTION", "direction"}, {"FLAGS", "flags"},
	{"INTERFACE", "rfInterface"}, {"RX_MODE", "rxMode"}, {"VERSION", "version"},
	{"LINK_SOURCE_ROLES", "linkSourceRoles"}, {"LINK_TARGET_ROLES", "linkTargetRoles"},
}

func entryOut(e *DevEntry, meta *Meta, raw bool) map[string]interface{} {
	addr := fmt.Sprint(e.Desc["ADDRESS"])
	o := map[string]interface{}{"address": addr, "interface": e.Iface}
	for _, f := range descFields {
		if v, ok := e.Desc[f.key]; ok && v != "" {
			o[f.out] = v
		}
	}
	if _, ok := e.Desc["PARENT"]; ok && e.Desc["PARENT"] != "" {
		o["index"] = toInt64(e.Desc["INDEX"])
		o["parent"] = e.Desc["PARENT"]
	}
	if meta != nil {
		if n, ok := meta.Names[addr]; ok {
			o["name"] = n
		}
		if id, ok := meta.IDs[addr]; ok {
			o["id"] = id
		}
		if r := meta.Rooms[addr]; len(r) > 0 {
			o["rooms"] = r
		}
		if f := meta.Functions[addr]; len(f) > 0 {
			o["functions"] = f
		}
	}
	if raw {
		o["raw"] = e.Desc
	}
	return o
}

func hasParamset(e *DevEntry, key string) bool {
	ps, _ := e.Desc["PARAMSETS"].([]interface{})
	for _, p := range ps {
		if fmt.Sprint(p) == key {
			return true
		}
	}
	return false
}

func isChannel(e *DevEntry) bool {
	p, ok := e.Desc["PARENT"]
	return ok && fmt.Sprint(p) != ""
}

func sortByName(list []map[string]interface{}) {
	sort.SliceStable(list, func(a, b int) bool {
		na, _ := list[a]["name"].(string)
		nb, _ := list[b]["name"].(string)
		if na != nb {
			if na == "" {
				return false
			}
			if nb == "" {
				return true
			}
			return strings.ToLower(na) < strings.ToLower(nb)
		}
		return fmt.Sprint(list[a]["address"]) < fmt.Sprint(list[b]["address"])
	})
}

// ---------- Werte typgerecht machen ----------

var warnedBad sync.Map // Adresse|Parameter -> bereits gemeldet

// normValue wandelt einen Rohwert passend zur Parameterbeschreibung.
// ok=false: Wert ist unbrauchbar (z.B. "" bei einer Zahl) und wird nicht weitergegeben.
func normValue(pd map[string]interface{}, v interface{}) (interface{}, bool) {
	if pd == nil {
		return v, true
	}
	t, _ := pd["TYPE"].(string)
	if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" && t != "STRING" {
		return nil, false
	}
	switch t {
	case "FLOAT":
		f, err := toFloat(v)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return f, true
	case "INTEGER", "ENUM":
		f, err := toFloat(v)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return int64(math.Round(f)), true
	case "BOOL", "ACTION":
		b, err := toBool(v)
		if err != nil {
			return nil, false
		}
		return b, true
	case "STRING":
		if v == nil {
			return "", true
		}
		if s, ok := v.(string); ok {
			return s, true
		}
		return fmt.Sprint(v), true
	}
	return v, true
}

func warnBad(addr, param string, v interface{}) {
	if _, seen := warnedBad.LoadOrStore(strings.ToUpper(addr)+"|"+param, true); !seen {
		log.Printf("Unbrauchbarer Wert %q für %s %s wird ignoriert (weitere Meldungen dazu werden unterdrückt)", fmt.Sprint(v), addr, param)
	}
}

// descNoRefresh liefert die VALUES-Beschreibung, ohne für unbekannte Adressen
// (z.B. CENTRAL-Pings) die Geräteliste neu zu laden.
func (c *CCU) descNoRefresh(addr string) map[string]interface{} {
	if c.Index(false)[strings.ToUpper(strings.TrimSpace(addr))] == nil {
		return nil
	}
	d, err := c.ParamsetDescription(addr, "VALUES")
	if err != nil {
		return nil
	}
	return d
}

// NormalizeEvent prüft einen Event-Wert gegen die Beschreibung des Datenpunkts.
func (c *CCU) NormalizeEvent(addr, param string, v interface{}) (interface{}, bool) {
	desc := c.descNoRefresh(addr)
	pd, _ := desc[param].(map[string]interface{})
	if pd == nil {
		// unbekannt: nur offensichtlich leere Werte verwerfen
		if s, ok := v.(string); ok && s == "" && desc != nil {
			warnBad(addr, param, v)
			return nil, false
		}
		return v, true
	}
	nv, ok := normValue(pd, v)
	if !ok {
		warnBad(addr, param, v)
	}
	return nv, ok
}

// normalizeSet bereinigt ein Paramset: Werte typgerecht, unbrauchbare entfernt.
func normalizeSet(addr string, desc, vals map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		pd, _ := desc[k].(map[string]interface{})
		nv, ok := normValue(pd, v)
		if !ok {
			warnBad(addr, k, v)
			continue
		}
		out[k] = nv
	}
	return out
}
