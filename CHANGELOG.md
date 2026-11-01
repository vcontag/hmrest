# Änderungen

Neue Einträge oben. Die Überschrift `## <Version>` muss zu `var version` in `main.go` passen –
der Abschnitt wird beim Release automatisch als Beschreibung übernommen.

## 1.6.1
- Update-Anzeige: Die CCU-WebUI zeigt unter *Zusatzsoftware* an, wenn auf GitHub eine neuere Version vorliegt
  (Abfrage höchstens einmal pro Stunde). Die HM-REST-Kachelseite zeigt den Hinweis mit Download-Link ebenfalls.
- Erstes Release über GitHub – diese Version einmal einspielen, danach werden Updates automatisch gemeldet.

## 1.6.0
- Wochenprogramme von Heizungsthermostaten (HM-CC-RT-DN, HM-TC-IT-WM-W-EU, HmIP): neue Schnittstelle
  `GET/PUT /api/devices/{adresse}/weekprogram`, Format (mit/ohne Profile P1–P3) wird automatisch erkannt.
- Wochenplan-Editor in der Oberfläche: Zeitleiste je Tag, Kopieren auf Werktage/Wochenende/alle Tage,
  aktives Profil umschalten. Alle geänderten Tage gehen in einer Funkübertragung ans Gerät.

## 1.5.2
- Werte werden anhand der Gerätebeschreibung typgerecht gewandelt; unbrauchbare Werte (z.B. `""` statt Zahl)
  werden nicht mehr weitergegeben. Behebt den Abbruch des Event-Streams in Home Assistant.
- Events werden entkoppelt verarbeitet, die CCU bekommt sofort Antwort.

## 1.5.1
- CUxD-Einträge mit `http://` werden automatisch auf BIN-RPC (`bin://…:8701`) umgestellt.
- Schnittstellen erkennen selbst, wenn sie BIN-RPC statt XML-RPC sprechen.

## 1.5.0
- BIN-RPC-Unterstützung: CUxD-Geräte lesen, schalten und live verfolgen (Callback auf Port 9127).
- Neue Standard-Schnittstellen werden in bestehende Konfigurationen übernommen.

## 1.4.0
- Kachel- und Diagnose-Seite der CCU nur mit gültiger WebUI-Anmeldung.
- API-Key gibt es nur noch mit WebUI-Sitzung oder aktuellem Key; „Oberfläche öffnen" meldet automatisch an.

## 1.3.0
- Neuer API-Key wird automatisch übernommen (Oberfläche und Diagnose-Seite).

## 1.2.0
- Diagnose-Seite in der CCU (Start/Stopp, Log, Ports, API-Key), Programmauswahl durch Ausprobieren.

## 1.1.0
- API-Key per Button, Kachel unter *Einstellungen → Systemsteuerung*.

## 1.0.0
- Erste Version: REST-API für Geräte, Parameter, Systemvariablen, Programme, Live-Events per SSE, Web-Oberfläche.
