# HM-REST – REST-API-Add-on für CCU3 / RaspberryMatic

Stellt alle Geräte, Kanäle und Parameter (VALUES und MASTER) von BidCos-RF, HmIP-RF,
BidCos-Wired und virtuellen Geräten sowie Systemvariablen, Programme, Räume und Gewerke
über eine JSON-REST-API bereit. Wertänderungen kommen live per Server-Sent Events.

## Installation
1. `hmrest-<version>.tar.gz` (unter *Releases*) in der WebUI unter *Einstellungen → Systemsteuerung → Zusatzsoftware* hochladen und installieren.
2. Firewall: *Einstellungen → Systemsteuerung → Firewall* → Port-Freigabe `8099`.
3. In der WebUI *Einstellungen → Systemsteuerung → HM-REST → Oberfläche öffnen* – dabei wird der API-Key erzeugt
   und automatisch übernommen (im Reiter *API* sichtbar).

## Sicherheit
HM-REST ist für das **Heimnetz** gebaut: Es spricht unverschlüsseltes HTTP, erlaubt standardmäßig Zugriffe von jeder
Webseite (CORS `*`) und überträgt den API-Key bei Live-Events in der Adresse.
- **Port 8099 niemals aus dem Internet erreichbar machen** (keine Portfreigabe im Router).
- Für den Zugriff von unterwegs ein VPN nutzen (z.B. WireGuard der FRITZ!Box).
- Wer nur liest, kann in der `config.json` `"readOnly": true` setzen; `allowScript` nur bei Bedarf einschalten.

## Authentifizierung
Header `X-API-Key: <key>` oder `Authorization: Bearer <key>` oder `?apikey=<key>`.

## Endpunkte
| Methode | Pfad | Zweck |
|---|---|---|
| GET | `/api/info` | Status, Schnittstellen |
| GET | `/api/devices?values=1\|full&paramsets=MASTER&interface=HmIP-RF&room=Küche` | alle Geräte inkl. Kanälen, Namen, Räumen, optional Werte |
| GET | `/api/devices/{adresse}?values=full` | ein Gerät oder Kanal |
| GET | `/api/devices/{kanal}/values` | alle Werte eines Kanals mit Typ, Einheit, Min/Max |
| PUT | `/api/devices/{kanal}/values` | mehrere Werte setzen `{"LEVEL":0.5,"STOP":true}` |
| GET | `/api/devices/{kanal}/values/{param}` | ein Wert |
| PUT/POST | `/api/devices/{kanal}/values/{param}` | Wert setzen: `{"value":…}`, nackter JSON-Wert oder `?value=` |
| GET | `/api/devices/{adr}/paramsets/{VALUES\|MASTER\|peer}[?full=1]` | Paramset lesen |
| PUT | `/api/devices/{adr}/paramsets/{key}` | Paramset schreiben (z.B. MASTER-Konfiguration) |
| GET | `/api/devices/{adr}/paramsets/{key}/description` | Paramset-Beschreibung |
| GET | `/api/devices/{adr}/weekprogram` | Wochenprogramm eines Thermostats (alle Profile, aktives Profil) |
| PUT | `/api/devices/{adr}/weekprogram` | Wochenprogramm schreiben, siehe unten |
| GET | `/api/rooms`, `/api/functions` | Räume / Gewerke |
| GET/PUT | `/api/sysvars`, `/api/sysvars/{id\|name}` | Systemvariablen |
| GET/PUT | `/api/programs`, `/api/programs/{id\|name}` `{"active":true}` | Programme |
| POST | `/api/programs/{id\|name}/run` | Programm ausführen |
| POST | `/api/rega` | HomeMatic-Script ausführen (nur mit `allowScript: true`) |
| GET | `/api/events?address=…&param=LEVEL,STATE` | Live-Events (SSE) |
| GET | `/api/events/last` | letzter bekannter Wert je Datenpunkt seit Start |

Werte werden anhand der Paramset-Beschreibung automatisch in den richtigen Typ gewandelt:
`"0,5"` → FLOAT, `"on"`/`1`/`true` → BOOL, Enum-Namen wie `"DOWN"` → Index, Bereich wird geprüft.

## Beispiele
```sh
curl -H "X-API-Key: KEY" "http://ccu:8099/api/devices?values=1"
curl -X PUT -H "X-API-Key: KEY" -d '{"value":true}' http://ccu:8099/api/devices/000A1B2C3D4E5F:3/values/STATE
curl -X PUT -H "X-API-Key: KEY" -d '0.3' http://ccu:8099/api/devices/000A1B2C3D4E5F:4/values/LEVEL
curl -X PUT -H "X-API-Key: KEY" -d '{"value":"anwesend"}' http://ccu:8099/api/sysvars/Anwesenheit
curl -N "http://ccu:8099/api/events?apikey=KEY"
```

## Konfiguration (`/usr/local/addons/hmrest/etc/config.json`)
`listen`, `apiKey`, `cors` (Standard `*`), `readOnly`, `allowScript`, `disableEvents`,
`interfaces` (URLs je Schnittstelle, erste erreichbare wird genutzt), `regaUrls`.
Nach Änderungen: Add-on in der WebUI neu starten.

Hinweis: MASTER-Werte schreiben überträgt Konfiguration ans Gerät (Duty Cycle, bei Batteriegeräten erst beim nächsten Aufwachen).

## Bauen
`./build.sh` (Go ≥ 1.22) erzeugt `dist/hmrest-<version>.tar.gz` für arm, arm64, amd64, 386. Tests: `go test ./...`

## Wochenprogramme (HM-CC-RT-DN, HM-TC-IT-WM-W-EU, HmIP-Thermostate)
Format und Speicherort (Gerät oder Kanal, mit/ohne Profil-Präfix `P1_`) werden automatisch erkannt.
In der Oberfläche: Gerät öffnen → *Wochenprogramm → Bearbeiten*.

```sh
curl -X PUT -H "X-API-Key: KEY" -H "Content-Type: application/json" \
  -d '{"profile":1,"activeProfile":1,"days":{
        "WORKDAYS":[{"end":"06:00","temp":17},{"end":"22:00","temp":21},{"end":"24:00","temp":17}],
        "WEEKEND": [{"end":"08:00","temp":17},{"end":"23:00","temp":21},{"end":"24:00","temp":17}]}}' \
  http://ccu:8099/api/devices/MEQ1234567/weekprogram
```
- `end` = Ende des Abschnitts (`"HH:MM"` oder Minuten), 5-Minuten-Raster, aufsteigend, letzter Abschnitt `24:00`.
- Tage: `MONDAY` … `SUNDAY`, `WORKDAYS`, `WEEKEND`, `ALL` (auch kommagetrennt, auch deutsch); Einzeltage überschreiben Gruppen.
- Temperaturen werden auf 0,5 °C gerundet und gegen den Gerätebereich geprüft.
- Alle Tage gehen in **einem** `putParamset` ans Gerät (schont den Duty Cycle).

## Änderungen
Siehe [CHANGELOG.md](CHANGELOG.md). Fertige Pakete gibt es unter *Releases*.

## Lizenz und Marken
[MIT](LICENSE) – Nutzung ohne Gewährleistung und Haftung.

Dieses Projekt ist kein offizielles Produkt der eQ-3 AG und steht in keiner Verbindung zu ihr.
HomeMatic und Homematic IP sind eingetragene Marken der eQ-3 AG.
