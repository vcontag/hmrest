package main

// Zugriff auf die Logikschicht (ReGaHss) per HomeMatic-Script:
// Namen, Räume, Gewerke, Systemvariablen und Programme.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Rega struct {
	urls []string
	http *http.Client
	mu   sync.Mutex
	url  string

	metaMu sync.Mutex
	meta   *Meta
}

func NewRega(urls []string) *Rega {
	return &Rega{urls: urls, http: &http.Client{Timeout: 30 * time.Second}}
}

func (r *Rega) URL() string { r.mu.Lock(); defer r.mu.Unlock(); return r.url }

func (r *Rega) Run(script string) (string, error) {
	body := utf8ToLatin1([]byte(script))
	r.mu.Lock()
	urls := r.urls
	if r.url != "" {
		urls = append([]string{r.url}, r.urls...)
	}
	r.mu.Unlock()
	var lastErr error
	for _, u := range urls {
		resp, err := r.http.Post(u, "text/plain", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("ReGa HTTP %d", resp.StatusCode)
			continue
		}
		r.mu.Lock()
		r.url = u
		r.mu.Unlock()
		if i := bytes.LastIndex(data, []byte("<xml>")); i >= 0 {
			data = data[:i]
		}
		return string(latin1ToUTF8(data)), nil
	}
	return "", httpErr(503, "ReGa nicht erreichbar: %v", lastErr)
}

func lines(out string) [][]string {
	var res [][]string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		res = append(res, strings.Split(l, "\t"))
	}
	return res
}

// ---------- Namen / Räume / Gewerke ----------

type Meta struct {
	Names     map[string]string
	IDs       map[string]string
	Rooms     map[string][]string
	Functions map[string][]string
	RoomList  map[string][]string
	FuncList  map[string][]string
	at        time.Time
}

const metaScript = `string sId; string sCh; object o; object c;
foreach(sId, root.Devices().EnumUsedIDs()) {
  o = dom.GetObject(sId);
  if (o) {
    WriteLine("D\t" # o.ID() # "\t" # o.Address() # "\t" # o.Name());
    foreach(sCh, o.Channels().EnumUsedIDs()) {
      c = dom.GetObject(sCh);
      if (c) { WriteLine("C\t" # c.ID() # "\t" # c.Address() # "\t" # c.Name()); }
    }
  }
}
foreach(sId, dom.GetObject(ID_ROOMS).EnumUsedIDs()) {
  o = dom.GetObject(sId);
  WriteLine("r\t" # o.Name());
  foreach(sCh, o.EnumUsedIDs()) { c = dom.GetObject(sCh); if (c) { WriteLine("R\t" # o.Name() # "\t" # c.Address()); } }
}
foreach(sId, dom.GetObject(ID_FUNCTIONS).EnumUsedIDs()) {
  o = dom.GetObject(sId);
  WriteLine("f\t" # o.Name());
  foreach(sCh, o.EnumUsedIDs()) { c = dom.GetObject(sCh); if (c) { WriteLine("F\t" # o.Name() # "\t" # c.Address()); } }
}`

// Standard-Räume/Gewerke der CCU sind Übersetzungsschlüssel.
var builtinNames = map[string]string{
	"${roomKitchen}": "Küche", "${roomLivingRoom}": "Wohnzimmer", "${roomBedroom}": "Schlafzimmer",
	"${roomOffice}": "Büro", "${roomBathroom}": "Badezimmer", "${roomGarage}": "Garage",
	"${roomGarden}": "Garten", "${roomHWR}": "Hauswirtschaftsraum", "${roomChildrensRoom1}": "Kinderzimmer 1",
	"${roomChildrensRoom2}": "Kinderzimmer 2", "${roomDiningRoom}": "Esszimmer", "${roomStaircase}": "Treppenhaus",
	"${roomCellar}": "Keller", "${roomGuestRoom}": "Gästezimmer", "${roomHallway}": "Flur",
	"${funcLight}": "Licht", "${funcHeating}": "Heizung", "${funcClimateControl}": "Klima",
	"${funcWeather}": "Wetter", "${funcLock}": "Verschluss", "${funcSecurity}": "Sicherheit",
	"${funcEnergy}": "Energiemanagement", "${funcEnvironment}": "Umwelt", "${funcButton}": "Taster",
	"${funcCentral}": "Zentral",
}

func niceName(s string) string {
	if n, ok := builtinNames[s]; ok {
		return n
	}
	return s
}

func (r *Rega) Meta(force bool) (*Meta, error) {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	if r.meta != nil && !force && time.Since(r.meta.at) < 60*time.Second {
		return r.meta, nil
	}
	out, err := r.Run(metaScript)
	if err != nil {
		if r.meta != nil {
			return r.meta, nil
		}
		return &Meta{Names: map[string]string{}, IDs: map[string]string{}, Rooms: map[string][]string{},
			Functions: map[string][]string{}, RoomList: map[string][]string{}, FuncList: map[string][]string{}}, err
	}
	m := &Meta{Names: map[string]string{}, IDs: map[string]string{}, Rooms: map[string][]string{},
		Functions: map[string][]string{}, RoomList: map[string][]string{}, FuncList: map[string][]string{}, at: time.Now()}
	for _, f := range lines(out) {
		switch {
		case (f[0] == "D" || f[0] == "C") && len(f) >= 4:
			addr := strings.ToUpper(f[2])
			m.IDs[addr] = f[1]
			m.Names[addr] = f[3]
		case f[0] == "r" && len(f) >= 2:
			n := niceName(f[1])
			if _, ok := m.RoomList[n]; !ok {
				m.RoomList[n] = []string{}
			}
		case f[0] == "f" && len(f) >= 2:
			n := niceName(f[1])
			if _, ok := m.FuncList[n]; !ok {
				m.FuncList[n] = []string{}
			}
		case f[0] == "R" && len(f) >= 3:
			n, a := niceName(f[1]), strings.ToUpper(f[2])
			m.RoomList[n] = append(m.RoomList[n], a)
			m.Rooms[a] = append(m.Rooms[a], n)
		case f[0] == "F" && len(f) >= 3:
			n, a := niceName(f[1]), strings.ToUpper(f[2])
			m.FuncList[n] = append(m.FuncList[n], a)
			m.Functions[a] = append(m.Functions[a], n)
		}
	}
	r.meta = m
	return m, nil
}

// ---------- Systemvariablen ----------

type SysVar struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Value     interface{} `json:"value"`
	ValueList []string    `json:"valueList,omitempty"`
	Unit      string      `json:"unit,omitempty"`
	Min       interface{} `json:"min,omitempty"`
	Max       interface{} `json:"max,omitempty"`
	Name0     string      `json:"name0,omitempty"`
	Name1     string      `json:"name1,omitempty"`
	Timestamp string      `json:"timestamp"`
}

const sysvarScript = `string sId; object o;
foreach(sId, dom.GetObject(ID_SYSTEM_VARIABLES).EnumUsedIDs()) {
  o = dom.GetObject(sId);
  if (o) {
    WriteLine("V\t" # o.ID() # "\t" # o.Name() # "\t" # o.ValueType() # "\t" # o.ValueSubType() # "\t" # o.ValueList() # "\t" # o.ValueUnit() # "\t" # o.Timestamp() # "\t" # o.ValueMin() # "\t" # o.ValueMax() # "\t" # o.ValueName0() # "\t" # o.ValueName1() # "\t" # o.Value());
  }
}`

func (r *Rega) SysVars() ([]SysVar, error) {
	out, err := r.Run(sysvarScript)
	if err != nil {
		return nil, err
	}
	res := []SysVar{}
	for _, f := range lines(out) {
		if f[0] != "V" || len(f) < 13 {
			continue
		}
		vt, _ := strconv.Atoi(f[3])
		st, _ := strconv.Atoi(f[4])
		raw := strings.Join(f[12:], "\t")
		sv := SysVar{ID: f[1], Name: niceName(f[2]), Unit: f[6], Timestamp: f[7], Name0: f[10], Name1: f[11]}
		switch {
		case vt == 2:
			sv.Type = "bool"
			if st == 6 {
				sv.Type = "alarm"
			}
			sv.Value = raw == "true" || raw == "1"
		case vt == 4:
			sv.Type = "float"
			sv.Value, _ = toFloat(raw)
			sv.Min, _ = toFloat(f[8])
			sv.Max, _ = toFloat(f[9])
		case vt == 16 && st == 29:
			sv.Type = "enum"
			sv.Value = toInt64(raw)
			if f[5] != "" {
				sv.ValueList = strings.Split(f[5], ";")
			}
		case vt == 16:
			sv.Type = "integer"
			sv.Value = toInt64(raw)
		default:
			sv.Type = "string"
			sv.Value = raw
		}
		res = append(res, sv)
	}
	sort.Slice(res, func(a, b int) bool { return strings.ToLower(res[a].Name) < strings.ToLower(res[b].Name) })
	return res, nil
}

func (r *Rega) FindSysVar(ref string) (*SysVar, error) {
	list, err := r.SysVars()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == ref || list[i].Name == ref {
			return &list[i], nil
		}
	}
	for i := range list {
		if strings.EqualFold(list[i].Name, ref) {
			return &list[i], nil
		}
	}
	return nil, httpErr(404, "Systemvariable %q nicht gefunden", ref)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *Rega) SetSysVar(ref string, raw interface{}) (*SysVar, error) {
	sv, err := r.FindSysVar(ref)
	if err != nil {
		return nil, err
	}
	var lit string
	switch sv.Type {
	case "bool", "alarm":
		b, err := toBool(raw)
		if err != nil {
			// auch Anzeigenamen (z.B. "anwesend") zulassen
			s := fmt.Sprint(raw)
			switch {
			case sv.Name1 != "" && strings.EqualFold(s, sv.Name1):
				b = true
			case sv.Name0 != "" && strings.EqualFold(s, sv.Name0):
				b = false
			default:
				return nil, httpErr(400, "%v", err)
			}
		}
		lit = strconv.FormatBool(b)
	case "float":
		f, err := toFloat(raw)
		if err != nil {
			return nil, httpErr(400, "%v", err)
		}
		lit = strconv.FormatFloat(f, 'f', -1, 64)
	case "enum":
		if s, ok := raw.(string); ok && !isDigits(strings.TrimSpace(s)) {
			idx := -1
			for i, v := range sv.ValueList {
				if strings.EqualFold(v, strings.TrimSpace(s)) {
					idx = i
				}
			}
			if idx < 0 {
				return nil, httpErr(400, "%q nicht in Werteliste %v", s, sv.ValueList)
			}
			lit = strconv.Itoa(idx)
		} else {
			f, err := toFloat(raw)
			if err != nil {
				return nil, httpErr(400, "%v", err)
			}
			lit = strconv.Itoa(int(f))
		}
	case "integer":
		f, err := toFloat(raw)
		if err != nil {
			return nil, httpErr(400, "%v", err)
		}
		lit = strconv.Itoa(int(f))
	default:
		s := fmt.Sprint(raw)
		if raw == nil {
			s = ""
		}
		if strings.ContainsAny(s, "\"\\") {
			return nil, httpErr(400, "Zeichen \" und \\ sind in Systemvariablen-Texten nicht erlaubt")
		}
		lit = `"` + s + `"`
	}
	out, err := r.Run(fmt.Sprintf(`object o = dom.GetObject(%s); if (o) { o.State(%s); Write("OK"); } else { Write("NOTFOUND"); }`, sv.ID, lit))
	if err != nil {
		return nil, err
	}
	if !strings.Contains(out, "OK") {
		return nil, httpErr(500, "ReGa hat den Wert nicht gesetzt (%s)", strings.TrimSpace(out))
	}
	return r.FindSysVar(sv.ID)
}

// ---------- Programme ----------

type Program struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Active      bool   `json:"active"`
	Visible     bool   `json:"visible"`
	LastExecute string `json:"lastExecute"`
}

const programScript = `string sId; object o;
foreach(sId, dom.GetObject(ID_PROGRAMS).EnumUsedIDs()) {
  o = dom.GetObject(sId);
  if (o) { WriteLine("P\t" # o.ID() # "\t" # o.Active() # "\t" # o.Visible() # "\t" # o.ProgramLastExecuteTime() # "\t" # o.Name()); }
}`

func (r *Rega) Programs() ([]Program, error) {
	out, err := r.Run(programScript)
	if err != nil {
		return nil, err
	}
	res := []Program{}
	for _, f := range lines(out) {
		if f[0] != "P" || len(f) < 6 {
			continue
		}
		res = append(res, Program{ID: f[1], Active: f[2] == "true", Visible: f[3] == "true", LastExecute: f[4], Name: strings.Join(f[5:], "\t")})
	}
	sort.Slice(res, func(a, b int) bool { return strings.ToLower(res[a].Name) < strings.ToLower(res[b].Name) })
	return res, nil
}

func (r *Rega) FindProgram(ref string) (*Program, error) {
	list, err := r.Programs()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == ref || strings.EqualFold(list[i].Name, ref) {
			return &list[i], nil
		}
	}
	return nil, httpErr(404, "Programm %q nicht gefunden", ref)
}

func (r *Rega) RunProgram(ref string) (*Program, error) {
	p, err := r.FindProgram(ref)
	if err != nil {
		return nil, err
	}
	if _, err := r.Run(fmt.Sprintf(`object o = dom.GetObject(%s); if (o) { o.ProgramExecute(); }`, p.ID)); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *Rega) SetProgramActive(ref string, active bool) (*Program, error) {
	p, err := r.FindProgram(ref)
	if err != nil {
		return nil, err
	}
	if _, err := r.Run(fmt.Sprintf(`object o = dom.GetObject(%s); if (o) { o.Active(%t); }`, p.ID, active)); err != nil {
		return nil, err
	}
	return r.FindProgram(p.ID)
}
