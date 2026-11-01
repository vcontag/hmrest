package main

// Wochenprogramme von Heizungsthermostaten (HM-CC-RT-DN, HM-TC-IT-WM-W-EU, HmIP-eTRV/WTH …).
//
// Die Programme stehen im MASTER-Paramset, je nach Gerät/Firmware als
//   ENDTIME_MONDAY_1 / TEMPERATURE_MONDAY_1            (ein Programm)
//   P1_ENDTIME_MONDAY_1 / P1_TEMPERATURE_MONDAY_1 …    (mehrere Profile)
// ENDTIME = Minuten ab Mitternacht (Ende des Abschnitts), letzter Abschnitt endet um 1440.

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var weekDays = []string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

var wpParamRe = regexp.MustCompile(`^(?:P([1-9])_)?(ENDTIME|TEMPERATURE)_(MONDAY|TUESDAY|WEDNESDAY|THURSDAY|FRIDAY|SATURDAY|SUNDAY)_(\d{1,2})$`)

// Tag-Namen und Gruppen, auch deutsch (für API und HA-Aktion)
var dayAliases = map[string][]string{
	"MONDAY": {"MONDAY"}, "MO": {"MONDAY"}, "MONTAG": {"MONDAY"},
	"TUESDAY": {"TUESDAY"}, "TU": {"TUESDAY"}, "DI": {"TUESDAY"}, "DIENSTAG": {"TUESDAY"},
	"WEDNESDAY": {"WEDNESDAY"}, "WE": {"WEDNESDAY"}, "MI": {"WEDNESDAY"}, "MITTWOCH": {"WEDNESDAY"},
	"THURSDAY": {"THURSDAY"}, "TH": {"THURSDAY"}, "DO": {"THURSDAY"}, "DONNERSTAG": {"THURSDAY"},
	"FRIDAY": {"FRIDAY"}, "FR": {"FRIDAY"}, "FREITAG": {"FRIDAY"},
	"SATURDAY": {"SATURDAY"}, "SA": {"SATURDAY"}, "SAMSTAG": {"SATURDAY"},
	"SUNDAY": {"SUNDAY"}, "SU": {"SUNDAY"}, "SO": {"SUNDAY"}, "SONNTAG": {"SUNDAY"},
	"WORKDAYS": weekDays[:5], "WEEKDAYS": weekDays[:5], "WERKTAGE": weekDays[:5], "MO-FR": weekDays[:5],
	"WEEKEND": weekDays[5:], "WOCHENENDE": weekDays[5:], "SA-SO": weekDays[5:],
	"ALL": weekDays, "ALLE": weekDays, "DAILY": weekDays, "TÄGLICH": weekDays, "TAEGLICH": weekDays,
}

func resolveDays(key string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(key, ",") {
		k := strings.ToUpper(strings.TrimSpace(part))
		if k == "" {
			continue
		}
		d, ok := dayAliases[k]
		if !ok {
			return nil, httpErr(400, "unbekannter Tag %q (MONDAY…SUNDAY, WORKDAYS, WEEKEND, ALL)", part)
		}
		out = append(out, d...)
	}
	if len(out) == 0 {
		return nil, httpErr(400, "kein Tag angegeben")
	}
	return out, nil
}

type WPSlot struct {
	End  string  `json:"end"`  // "HH:MM", letzter Abschnitt "24:00"
	Temp float64 `json:"temp"` // °C
}

type WeekProgramInfo struct {
	Device        string                         `json:"device"`
	Address       string                         `json:"address"` // wo das Programm liegt
	Name          string                         `json:"name,omitempty"`
	Type          string                         `json:"type,omitempty"`
	Prefixed      bool                           `json:"prefixed"`
	Profiles      []int                          `json:"profiles"`
	ActiveProfile *int                           `json:"activeProfile"`
	Slots         int                            `json:"slots"`
	MinTemp       float64                        `json:"minTemp"`
	MaxTemp       float64                        `json:"maxTemp"`
	Program       map[string]map[string][]WPSlot `json:"program"` // Profil -> Tag -> Abschnitte

	desc        map[string]interface{}
	pointerKind string // "master" (WEEK_PROGRAM_POINTER) oder "values" (ACTIVE_PROFILE)
	pointerMin  int
}

func fmtMinutes(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// parseEnd akzeptiert "06:30", "6:30", "24:00" oder Minuten als Zahl.
func parseEnd(v interface{}) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(math.Round(x)), nil
	case int64:
		return int(x), nil
	case int:
		return x, nil
	case string:
		s := strings.TrimSpace(x)
		if h, m, ok := strings.Cut(s, ":"); ok {
			hh, e1 := strconv.Atoi(h)
			mm, e2 := strconv.Atoi(m)
			if e1 != nil || e2 != nil || mm < 0 || mm > 59 {
				return 0, fmt.Errorf("ungültige Uhrzeit %q", x)
			}
			return hh*60 + mm, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("ungültige Uhrzeit %q", x)
		}
		return n, nil
	}
	return 0, fmt.Errorf("ungültige Uhrzeit %v", v)
}

// findWeekProgram sucht im Gerät und seinen Kanälen das MASTER-Paramset mit Wochenprogramm.
func (c *CCU) findWeekProgram(addr string) (*WeekProgramInfo, error) {
	e, _, err := c.Lookup(addr)
	if err != nil {
		return nil, err
	}
	dev := e
	if isChannel(e) {
		if p, _, err := c.Lookup(fmt.Sprint(e.Desc["PARENT"])); err == nil {
			dev = p
		}
	}
	cands := []*DevEntry{}
	if isChannel(e) {
		cands = append(cands, e) // angegebener Kanal zuerst
	}
	cands = append(cands, dev)
	children, _ := dev.Desc["CHILDREN"].([]interface{})
	for _, ch := range children {
		if ce, _, err := c.Lookup(fmt.Sprint(ch)); err == nil && ce != e {
			cands = append(cands, ce)
		}
	}
	for _, cand := range cands {
		if !hasParamset(cand, "MASTER") {
			continue
		}
		a := fmt.Sprint(cand.Desc["ADDRESS"])
		desc, err := c.ParamsetDescription(a, "MASTER")
		if err != nil {
			continue
		}
		info := &WeekProgramInfo{Device: fmt.Sprint(dev.Desc["ADDRESS"]), Address: a, Type: fmt.Sprint(dev.Desc["TYPE"]), desc: desc,
			MinTemp: 4.5, MaxTemp: 30.5}
		profiles := map[int]bool{}
		minT, maxT := math.Inf(1), math.Inf(-1)
		for name, d := range desc {
			m := wpParamRe.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			p := 1
			if m[1] != "" {
				p, _ = strconv.Atoi(m[1])
				info.Prefixed = true
			}
			profiles[p] = true
			n, _ := strconv.Atoi(m[4])
			if n > info.Slots {
				info.Slots = n
			}
			if m[2] == "TEMPERATURE" {
				if dm, ok := d.(map[string]interface{}); ok {
					if v, err := toFloat(dm["MIN"]); err == nil {
						minT = math.Min(minT, v)
					}
					if v, err := toFloat(dm["MAX"]); err == nil {
						maxT = math.Max(maxT, v)
					}
				}
			}
		}
		if info.Slots == 0 {
			continue
		}
		if !math.IsInf(minT, 0) && !math.IsInf(maxT, 0) && minT < maxT {
			info.MinTemp, info.MaxTemp = minT, maxT
		}
		for p := range profiles {
			info.Profiles = append(info.Profiles, p)
		}
		sort.Ints(info.Profiles)
		if pd, ok := desc["WEEK_PROGRAM_POINTER"].(map[string]interface{}); ok {
			info.pointerKind = "master"
			info.pointerMin = int(toInt64(pd["MIN"]))
		} else if vd, err := c.ParamsetDescription(a, "VALUES"); err == nil {
			if _, ok := vd["ACTIVE_PROFILE"]; ok {
				info.pointerKind = "values"
			}
		}
		return info, nil
	}
	return nil, httpErr(404, "%s hat kein Wochenprogramm", addr)
}

func (info *WeekProgramInfo) paramName(profile int, kind, day string, n int) string {
	if info.Prefixed {
		return fmt.Sprintf("P%d_%s_%s_%d", profile, kind, day, n)
	}
	return fmt.Sprintf("%s_%s_%d", kind, day, n)
}

// GetWeekProgram liest das komplette Programm aller Profile.
func (c *CCU) GetWeekProgram(addr string) (*WeekProgramInfo, error) {
	info, err := c.findWeekProgram(addr)
	if err != nil {
		return nil, err
	}
	vals, err := c.GetParamset(info.Address, "MASTER")
	if err != nil {
		return nil, err
	}
	info.Program = map[string]map[string][]WPSlot{}
	for _, p := range info.Profiles {
		days := map[string][]WPSlot{}
		for _, day := range weekDays {
			slots := []WPSlot{}
			for n := 1; n <= info.Slots; n++ {
				ev, ok1 := vals[info.paramName(p, "ENDTIME", day, n)]
				tv, ok2 := vals[info.paramName(p, "TEMPERATURE", day, n)]
				if !ok1 || !ok2 {
					break
				}
				end, err := parseEnd(ev)
				if err != nil {
					break
				}
				t, _ := toFloat(tv)
				if end > 1440 {
					end = 1440
				}
				slots = append(slots, WPSlot{End: fmtMinutes(end), Temp: t})
				if end >= 1440 {
					break
				}
			}
			days[day] = slots
		}
		info.Program[strconv.Itoa(p)] = days
	}
	switch info.pointerKind {
	case "master":
		if v, ok := vals["WEEK_PROGRAM_POINTER"]; ok {
			ap := int(toInt64(v)) - info.pointerMin + 1
			info.ActiveProfile = &ap
		}
	case "values":
		if v, err := c.GetValue(info.Address, "ACTIVE_PROFILE"); err == nil && v != nil {
			ap := int(toInt64(v))
			info.ActiveProfile = &ap
		}
	}
	if meta, err := c.Rega.Meta(false); err == nil {
		info.Name = meta.Names[strings.ToUpper(info.Device)]
	}
	return info, nil
}

// WeekProgramUpdate ist der Body von PUT …/weekprogram.
type WeekProgramUpdate struct {
	Profile       int                         `json:"profile"`
	Days          map[string][]map[string]any `json:"days"`
	ActiveProfile *int                        `json:"activeProfile"`
}

// SetWeekProgram prüft den Plan und schreibt alle Tage in einem einzigen putParamset.
func (c *CCU) SetWeekProgram(addr string, u WeekProgramUpdate) (map[string]interface{}, error) {
	info, err := c.findWeekProgram(addr)
	if err != nil {
		return nil, err
	}
	profile := u.Profile
	if profile == 0 {
		profile = info.Profiles[0]
	}
	okProfile := false
	for _, p := range info.Profiles {
		okProfile = okProfile || p == profile
	}
	if !okProfile {
		return nil, httpErr(400, "Profil %d gibt es nicht (vorhanden: %v)", profile, info.Profiles)
	}
	if len(u.Days) == 0 && u.ActiveProfile == nil {
		return nil, httpErr(400, "keine Tage übergeben")
	}
	if u.ActiveProfile != nil {
		if info.pointerKind == "" {
			return nil, httpErr(400, "dieses Gerät kennt keine Profilumschaltung")
		}
		ok := false
		for _, p := range info.Profiles {
			ok = ok || p == *u.ActiveProfile
		}
		if !ok {
			return nil, httpErr(400, "aktives Profil %d gibt es nicht (vorhanden: %v)", *u.ActiveProfile, info.Profiles)
		}
	}

	values := map[string]interface{}{}
	written := map[string][]WPSlot{}
	// Gruppen zuerst, einzelne Tage überschreiben sie (z.B. ALL + SUNDAY)
	keys := make([]string, 0, len(u.Days))
	for k := range u.Days {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(a, b int) bool {
		da, _ := resolveDays(keys[a])
		db, _ := resolveDays(keys[b])
		return len(da) > len(db)
	})
	for _, key := range keys {
		days, err := resolveDays(key)
		if err != nil {
			return nil, err
		}
		raw := u.Days[key]
		if len(raw) == 0 {
			return nil, httpErr(400, "%s: keine Abschnitte", key)
		}
		if len(raw) > info.Slots {
			return nil, httpErr(400, "%s: höchstens %d Abschnitte pro Tag möglich", key, info.Slots)
		}
		slots := make([]WPSlot, 0, len(raw))
		ends := make([]int, 0, len(raw))
		temps := make([]float64, 0, len(raw))
		prev := 0
		for i, s := range raw {
			end, err := parseEnd(s["end"])
			if err != nil {
				return nil, httpErr(400, "%s, Abschnitt %d: %v", key, i+1, err)
			}
			if end%5 != 0 {
				return nil, httpErr(400, "%s, Abschnitt %d: Uhrzeit %s muss auf 5 Minuten enden", key, i+1, fmtMinutes(end))
			}
			if end <= prev || end > 1440 {
				return nil, httpErr(400, "%s, Abschnitt %d: Uhrzeiten müssen aufsteigend sein und höchstens 24:00 betragen", key, i+1)
			}
			t, err := toFloat(s["temp"])
			if err != nil {
				return nil, httpErr(400, "%s, Abschnitt %d: Temperatur fehlt oder ist ungültig", key, i+1)
			}
			t = math.Round(t*2) / 2 // Thermostate arbeiten in 0,5-°C-Schritten
			if t < info.MinTemp || t > info.MaxTemp {
				return nil, httpErr(400, "%s, Abschnitt %d: %.1f °C außerhalb %.1f…%.1f °C", key, i+1, t, info.MinTemp, info.MaxTemp)
			}
			prev = end
			ends, temps = append(ends, end), append(temps, t)
			slots = append(slots, WPSlot{End: fmtMinutes(end), Temp: t})
		}
		if prev != 1440 {
			return nil, httpErr(400, "%s: der letzte Abschnitt muss um 24:00 enden", key)
		}
		for _, day := range days {
			for n := 1; n <= info.Slots; n++ {
				end, t := 1440, temps[len(temps)-1] // unbenutzte Abschnitte wie die CCU füllen
				if n <= len(ends) {
					end, t = ends[n-1], temps[n-1]
				}
				values[info.paramName(profile, "ENDTIME", day, n)] = end
				values[info.paramName(profile, "TEMPERATURE", day, n)] = t
			}
			written[day] = slots
		}
	}

	if u.ActiveProfile != nil && info.pointerKind == "master" {
		values["WEEK_PROGRAM_POINTER"] = *u.ActiveProfile - 1 + info.pointerMin
	}
	if len(values) > 0 {
		if _, err := c.PutParamset(info.Address, "MASTER", values); err != nil {
			return nil, err
		}
	}
	if u.ActiveProfile != nil {
		switch info.pointerKind {
		case "values":
			if _, err := c.SetValue(info.Address, "ACTIVE_PROFILE", *u.ActiveProfile); err != nil {
				return nil, err
			}
		}
	}
	return map[string]interface{}{"ok": true, "address": info.Address, "profile": profile, "days": written,
		"parameters": len(values), "activeProfile": u.ActiveProfile,
		"hint": "Batteriegeräte übernehmen die Konfiguration verzögert (CONFIG_PENDING)"}, nil
}

// ---------- HTTP ----------

func (a *API) getWeekProgram(w http.ResponseWriter, r *http.Request) {
	info, err := a.ccu.GetWeekProgram(r.PathValue("address"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, info)
}

func (a *API) putWeekProgram(w http.ResponseWriter, r *http.Request) {
	m, err := readObject(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var u WeekProgramUpdate
	if p, ok := m["profile"]; ok {
		u.Profile = int(toInt64(p))
	}
	if ap, ok := m["activeProfile"]; ok && ap != nil {
		v := int(toInt64(ap))
		u.ActiveProfile = &v
	}
	u.Days = map[string][]map[string]any{}
	if d, ok := m["days"].(map[string]interface{}); ok {
		for k, v := range d {
			arr, ok := v.([]interface{})
			if !ok {
				writeErr(w, httpErr(400, "%s: Liste von {end, temp} erwartet", k))
				return
			}
			for _, s := range arr {
				sm, ok := s.(map[string]interface{})
				if !ok {
					writeErr(w, httpErr(400, "%s: Abschnitt muss {\"end\":\"06:00\",\"temp\":21} sein", k))
					return
				}
				u.Days[k] = append(u.Days[k], sm)
			}
		}
	}
	res, err := a.ccu.SetWeekProgram(r.PathValue("address"), u)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}
