package main

// HM-REST – REST-API-Add-on für HomeMatic CCU3 / RaspberryMatic.

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "1.6.1"

//go:embed web/index.html
var webFS embed.FS

type Config struct {
	Listen         string            `json:"listen"`
	APIKey         string            `json:"apiKey"`
	NoAuth         bool              `json:"noAuth"`
	CORS           string            `json:"cors"`
	ReadOnly       bool              `json:"readOnly"`
	AllowScript    bool              `json:"allowScript"`
	DisableEvents  bool              `json:"disableEvents"`
	CallbackListen string            `json:"callbackListen"`
	CallbackURL    string            `json:"callbackUrl"`
	BinCallback    string            `json:"binCallbackListen"`
	BinCallbackURL string            `json:"binCallbackUrl"`
	ReinitMinutes  int               `json:"reinitMinutes"`
	RegaURLs       []string          `json:"regaUrls"`
	SessionURLs    []string          `json:"sessionUrls"`
	Interfaces     []InterfaceConfig `json:"interfaces"`
}

func defaultConfig() *Config {
	return &Config{
		Listen:         ":8099",
		CORS:           "*",
		CallbackListen: "127.0.0.1:9126",
		CallbackURL:    "http://127.0.0.1:9126",
		BinCallback:    "127.0.0.1:9127",
		BinCallbackURL: "xmlrpc_bin://127.0.0.1:9127",
		ReinitMinutes:  10,
		// 8183 = interner ReGa-Port (CCU3/RaspberryMatic), 8181 = über lighttpd
		RegaURLs: []string{"http://127.0.0.1:8183/rega.exe", "http://127.0.0.1:8181/rega.exe"},
		// JSON-API der WebUI zur Prüfung der Anmeldung
		SessionURLs: []string{"http://127.0.0.1/api/homematic.cgi", "https://127.0.0.1/api/homematic.cgi"},
		// Interne Ports (3xxxx) zuerst, dann die öffentlichen Ports als Fallback
		Interfaces: []InterfaceConfig{
			{Name: "BidCos-RF", URLs: []string{"http://127.0.0.1:32001", "http://127.0.0.1:2001"}},
			{Name: "HmIP-RF", URLs: []string{"http://127.0.0.1:32010", "http://127.0.0.1:2010"}},
			{Name: "BidCos-Wired", URLs: []string{"http://127.0.0.1:32000", "http://127.0.0.1:2000"}},
			{Name: "VirtualDevices", URLs: []string{"http://127.0.0.1:39292/groups", "http://127.0.0.1:9292/groups"}},
			// CUxD spricht BIN-RPC
			{Name: "CUxD", URLs: []string{"bin://127.0.0.1:8701"}},
		},
	}
}

func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	save := false
	if errors.Is(err, os.ErrNotExist) {
		save = true
	} else if err != nil {
		return nil, err
	} else if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Neue Standard-Schnittstellen (z.B. CUxD) in bestehende Konfigurationen übernehmen.
	// Wer eine nicht will, setzt dort "enabled": false (löschen hilft nicht).
	have := map[string]bool{}
	for _, ic := range cfg.Interfaces {
		have[strings.ToLower(ic.Name)] = true
	}
	for _, ic := range defaultConfig().Interfaces {
		if !have[strings.ToLower(ic.Name)] {
			cfg.Interfaces = append(cfg.Interfaces, ic)
			log.Printf("Schnittstelle %s zur Konfiguration hinzugefügt", ic.Name)
			save = true
		}
	}
	// CUxD spricht nur BIN-RPC: http://-Einträge (Port 8701 oder Name CUxD) umstellen
	for n := range cfg.Interfaces {
		ic := &cfg.Interfaces[n]
		for k, u := range ic.URLs {
			if isBinURL(u) {
				continue
			}
			bu := binURLFor(u)
			if bu != "" && (strings.EqualFold(ic.Name, "CUxD") || strings.HasSuffix(bu, ":8701")) {
				log.Printf("Schnittstelle %s: %s → %s (CUxD spricht BIN-RPC)", ic.Name, u, bu)
				ic.URLs[k] = bu
				save = true
			}
		}
	}
	// Kein API-Key: wird über die Web-Oberfläche per Button erzeugt.
	if cfg.ReinitMinutes <= 0 {
		cfg.ReinitMinutes = 10
	}
	if save {
		if err := saveConfig(path, cfg); err != nil {
			log.Printf("Konfiguration konnte nicht gespeichert werden: %v", err)
		} else {
			log.Printf("Konfiguration geschrieben: %s", path)
		}
	}
	return cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rotWriter begrenzt die Logdatei (CCU: /var/log liegt im RAM).
type rotWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
}

func newRotWriter(path string, max int64) (*rotWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	return &rotWriter{path: path, f: f, size: st.Size(), max: max}, nil
}

func (w *rotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.max {
		w.f.Close()
		_ = os.Rename(w.path, w.path+".1")
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		w.f, w.size = f, 0
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func main() {
	cfgPath := flag.String("config", "config.json", "Pfad zur Konfigurationsdatei")
	logPath := flag.String("log", "", "Logdatei (leer = stderr)")
	showVer := flag.Bool("version", false, "Version ausgeben")
	checkSID := flag.String("checksession", "", "WebUI-Sitzung prüfen (Exit-Code 0 = gültig), für die CCU-Seiten")
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	if *checkSID != "" {
		cfg := defaultConfig()
		if data, err := os.ReadFile(*cfgPath); err == nil {
			_ = json.Unmarshal(data, cfg)
		}
		if NewSessionChecker(cfg.SessionURLs, NewRega(cfg.RegaURLs)).Valid(*checkSID) {
			os.Exit(0)
		}
		os.Exit(1)
	}
	if *logPath != "" {
		if w, err := newRotWriter(*logPath, 512*1024); err == nil {
			log.SetOutput(io.MultiWriter(w))
		}
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("Konfiguration: %v", err)
	}

	ccu := NewCCU(cfg)
	hub := NewHub()
	api := &API{cfg: cfg, cfgPath: *cfgPath, ccu: ccu, hub: hub, started: time.Now(),
		sessions: NewSessionChecker(cfg.SessionURLs, ccu.Rega)}
	if cfg.APIKey == "" && !cfg.NoAuth {
		log.Printf("Noch kein API-Key – bitte über die CCU-WebUI (Systemsteuerung → HM-REST) erzeugen")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !cfg.DisableEvents {
		api.cb = NewCallback(ccu, hub, cfg.CallbackListen, cfg.CallbackURL, time.Duration(cfg.ReinitMinutes)*time.Minute)
		api.cb.SetBin(cfg.BinCallback, cfg.BinCallbackURL)
		go api.cb.ListenAndServe()
		go func() {
			time.Sleep(2 * time.Second) // Callback-Server zuerst starten
			api.cb.Run(ctx)
		}()
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: api.Middleware(api.Routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("HM-REST %s lauscht auf %s", version, cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP-Server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("Beende …")
	if api.cb != nil {
		api.cb.Deregister()
	}
	hub.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}
