# HM-REST – CCU-Add-on (Go)

REST-API + Web-Oberfläche für HomeMatic CCU3/RaspberryMatic. Läuft als Add-on direkt auf der CCU,
ein einzelnes statisches Go-Binary ohne Abhängigkeiten (nur Standardbibliothek).
Gegenstück: Home-Assistant-Integration im Repo `vcontag/hmrest-homeassistant` (nutzt `/api/devices`, `/api/events`,
`/api/sysvars`, `/api/devices/{adr}/weekprogram`). API-Änderungen dort mitdenken, abwärtskompatibel bleiben.

## Befehle
- Tests: `go test -race ./...` (alle Tests laufen gegen simulierte CCU-Dienste, keine Hardware nötig)
- Prüfen: `gofmt -l . && go vet ./...`
- Paket bauen: `./build.sh` → `dist/hmrest-<version>.tar.gz` (arm/arm64/amd64/386, CGO_ENABLED=0, ARM mit GOARM=5)
- Version: `var version` in `main.go` – bei jeder Auslieferung erhöhen (wird auch in `VERSION` des Pakets geschrieben)

## Aufbau
- `main.go` – Konfiguration (`config.json`, Migration fehlender Standard-Schnittstellen, CUxD http→bin), Start, `-checksession`
- `xmlrpc.go` – XML-RPC-Client/-Codec (CCU spricht ISO-8859-1!)
- `binrpc.go` – BIN-RPC (CUxD auf Port 8701 spricht NUR BIN-RPC) inkl. Callback-Server
- `ccu.go` – Schnittstellen (BidCos-RF, HmIP-RF, BidCos-Wired, VirtualDevices, CUxD), Geräteindex, Paramset-Zugriff,
  Typwandlung `convertValue` (Schreiben) und `normValue` (Lesen/Events: z.B. `""` bei FLOAT wird verworfen)
- `events.go` – `init()`-Anmeldung bei den Schnittstellen, Callback-Empfang (XML-RPC 9126, BIN-RPC 9127),
  Worker-Queue → `Hub` → SSE `/api/events`
- `rega.go` – HomeMatic-Script über rega.exe: Namen/Räume/Gewerke, Systemvariablen, Programme
- `session.go` – Prüfung der WebUI-Sitzung (`Session.renewSession` über JSON-API, Fallback ReGa)
- `weekprogram.go` – Wochenprogramme von Thermostaten (Format-Erkennung `P1_ENDTIME_MONDAY_1` / `ENDTIME_MONDAY_1`)
- `api.go` – HTTP-Routen, API-Key-Auth, `/setup/*` (Key nur mit WebUI-Sitzung oder aktuellem Key)
- `web/index.html` – eingebettete Oberfläche (Vanilla JS, mobil-first, eine Datei)
- `addon/` – Paketinhalt: `update_script` (Installation, wählt lauffähiges Binary durch Ausprobieren),
  `rc.d/hmrest` (Start/Stopp), `hmrest/www` (CCU-Seiten, nur Tcl-Aufrufer + Icon), `hmrest/lib` (Shell-Seiten
  mit Sitzungsprüfung), `hmrest/etc` (Standardkonfiguration, `hm_addons.cfg` für die Kachel)

## Regeln / Stolperfallen
- Keine externen Go-Abhängigkeiten – das Binary muss auf CCU2/CCU3/RaspberryMatic laufen.
- Callbacks der CCU nie synchron mit RPC-Aufrufen an dieselbe Schnittstelle beantworten (Deadlock-Gefahr, v.a. CUxD) –
  daher die Event-Queue.
- MASTER-Schreibzugriffe kosten Duty Cycle: immer gebündelt in einem `putParamset`.
- Werte aus der CCU nie ungeprüft an Clients geben – über die Paramset-Beschreibung normalisieren.
- CCU-Seiten (`addon/hmrest/lib/*.sh`) laufen mit BusyBox-`sh`/`sed`: keine Bash-Erweiterungen.
- Oberflächen- und Fehlertexte auf Deutsch.
- README behält den Sicherheitshinweis (nur Heimnetz, Port 8099 nie ins Internet) und den Markenhinweis
  (kein offizielles eQ-3-Produkt, HomeMatic ist Marke der eQ-3 AG).
- Konfiguration und API-Key bleiben bei Updates erhalten (`update_script` überschreibt `config.json` nicht).

## Auf der CCU
- Installiert unter `/usr/local/addons/hmrest`, Log `/var/log/hmrest.log`, Port 8099 (Firewall-Freigabe nötig)
- Diagnose: *Einstellungen → Systemsteuerung → HM-REST → Diagnose* (Start/Stopp, Log, Ports, neuer API-Key)
- Update-Anzeige: `rc.d/hmrest info` meldet `Update: /addons/hmrest/update-check.cgi`; `lib/update-check.sh` fragt
  `api.github.com/repos/vcontag/hmrest/releases/latest` ab (1 h Cache in `/tmp`). Funktioniert nur bei öffentlichem Repo.

## Releases
- Ein Release entsteht automatisch, wenn auf `main` eine Versionsnummer landet, für die es noch kein Release gibt
  (Job `release` in `.github/workflows/ci.yml`, nur nach grünen Tests).
- Für ein neues Release: Version erhöhen (`var version` in `main.go`) **und** oben in `CHANGELOG.md` einen Abschnitt `## <Version>`
  ergänzen – der wird als Release-Beschreibung übernommen. Ohne Versionssprung passiert nichts.
- Das Paket (`hmrest-<version>.tar.gz`) hängt am Release unter *Releases* auf GitHub.
