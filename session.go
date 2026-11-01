package main

// Prüfung einer HomeMatic-WebUI-Sitzung (sid=@XXXXXXXXXX@).
// Gültig ist eine Sitzung, wenn die JSON-API der CCU sie verlängern kann
// (Session.renewSession) oder ReGa eine Sitzungsvariable dazu kennt.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var sidRe = regexp.MustCompile(`^(?:@|%40)?([0-9A-Za-z]{10})(?:@|%40)?$`)

// normalizeSID liefert die 10 Zeichen der Sitzungs-ID oder "".
func normalizeSID(s string) string {
	m := sidRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	return m[1]
}

type SessionChecker struct {
	jsonURLs []string
	rega     *Rega
	http     *http.Client

	mu    sync.Mutex
	valid map[string]time.Time
}

func NewSessionChecker(jsonURLs []string, rega *Rega) *SessionChecker {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// nur für 127.0.0.1: Die CCU nutzt ein selbstsigniertes Zertifikat
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &SessionChecker{
		jsonURLs: jsonURLs, rega: rega, valid: map[string]time.Time{},
		http: &http.Client{
			Timeout: 8 * time.Second, Transport: tr,
			// keine Weiterleitung folgen (POST würde zu GET)
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Valid prüft die Sitzung; gültige Sitzungen werden 60 s zwischengespeichert.
func (s *SessionChecker) Valid(raw string) bool {
	sid := normalizeSID(raw)
	if sid == "" {
		return false
	}
	s.mu.Lock()
	if t, ok := s.valid[sid]; ok && time.Since(t) < 60*time.Second {
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()

	ok := s.viaJSON(sid) || s.viaRega(sid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.valid[sid] = time.Now()
	} else {
		delete(s.valid, sid)
	}
	return ok
}

func (s *SessionChecker) viaJSON(sid string) bool {
	body, _ := json.Marshal(map[string]interface{}{
		"version": "1.1", "method": "Session.renewSession", "id": 1,
		"params": map[string]string{"_session_id_": sid},
	})
	for _, u := range s.jsonURLs {
		resp, err := s.http.Post(u, "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}
		var r struct {
			Result interface{} `json:"result"`
		}
		err = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if err != nil {
			continue
		}
		return r.Result == true
	}
	return false
}

func (s *SessionChecker) viaRega(sid string) bool {
	if s.rega == nil {
		return false
	}
	// sid ist durch normalizeSID auf [0-9A-Za-z]{10} beschränkt
	out, err := s.rega.Run(fmt.Sprintf(`Write(system.GetSessionVarStr('%s'));`, sid))
	return err == nil && strings.TrimSpace(out) != ""
}
