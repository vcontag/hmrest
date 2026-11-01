#!/bin/sh
# HM-REST Diagnose-Seite – wird von www/diagnose.cgi aufgerufen
. ${HMREST_ADDON:-/usr/local/addons/hmrest}/lib/common.sh
require_session

ACTION=$(qparam action)
RESULT=""
NEWKEY=""
case "${ACTION}" in
  start)    RESULT=$(${RC} restart 2>&1) ;;
  stop)     RESULT=$(${RC} stop 2>&1) ;;
  newkey)
    NEWKEY=$(tr -d '-' < /proc/sys/kernel/random/uuid 2>/dev/null)
    [ ${#NEWKEY} -eq 32 ] || NEWKEY=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
    if [ ${#NEWKEY} -ge 32 ] && grep -q '"apiKey"' ${CONFIG}; then
      sed -i "s/\"apiKey\": *\"[^\"]*\"/\"apiKey\": \"${NEWKEY}\"/" ${CONFIG}
      RESULT="Neuer API-Key gespeichert. $(${RC} restart 2>&1)"
    else
      NEWKEY=""
      RESULT="API-Key konnte nicht erzeugt werden (config.json prüfen)."
    fi ;;
esac

STATUS=$(${RC} status 2>&1)

page_head "HM-REST Diagnose"
echo "<h2>HM-REST – Diagnose</h2>"
if [ "${STATUS}" = "running" ]; then
  echo "<p class='ok'>● Das Add-on läuft.</p>"
else
  echo "<p class='bad'>● Das Add-on läuft nicht.</p>"
fi
cat <<EOF
<a class="b" href="diagnose.cgi?${SIDQ}&action=start">Starten / Neu starten</a>
<a class="b g" href="diagnose.cgi?${SIDQ}&action=stop">Stoppen</a>
<a class="b g" href="diagnose.cgi?${SIDQ}">Aktualisieren</a>
<a class="b g" href="index.cgi?${SIDQ}">Zurück</a>
EOF
if [ -n "${RESULT}" ]; then
  echo "<h3>Ergebnis</h3><pre>"; echo "${RESULT}" | esc; echo "</pre>"
fi
if [ -n "${NEWKEY}" ]; then
  cat <<EOF
<h3>Neuer API-Key</h3>
<pre>${NEWKEY}</pre>
<a class="b" id="openui" href="#">Oberfläche öffnen und Key übernehmen</a>
<script>
document.getElementById('openui').href = 'http://' + location.hostname + ':8099/#sid=@${SID}@';
</script>
<p class="s">Der Key wird in der Oberfläche automatisch eingetragen.
Skripte oder Home Assistant, die den alten Key nutzen, musst du anpassen.</p>
EOF
fi
section() { echo "<h3>$1</h3><pre>"; esc; echo "</pre>"; }

( wget -q -T 3 -O - http://127.0.0.1:8099/setup/status 2>&1 || echo "(keine Antwort auf 127.0.0.1:8099)" ) | section "Antwort lokal (127.0.0.1:8099)"
( echo "Architektur: $(uname -m)"; echo "Gewähltes Programm: $(cat ${ADDON}/ARCH 2>/dev/null)";
  echo "Version: $(${ADDON}/bin/hmrest -version 2>&1)" ) | section "Programm"
( netstat -tln 2>/dev/null | grep -E ':(8099|9126|9127|8701) ' || echo "(Port 8099 ist nicht offen)" ) | section "Offene Ports"
( tail -n 40 ${LOG} 2>/dev/null || echo "(noch keine Logdatei)" ) | section "Log (letzte 40 Zeilen)"
( ls -la ${ADDON} ${ADDON}/bin ${ADDON}/etc 2>&1 ) | section "Dateien"
( mount | grep -E ' /usr/local | /var | /tmp ' ) | section "Mounts"
( df -h ${ADDON} 2>&1 ) | section "Speicher"

cat <<EOF
<h3>API-Key</h3>
<p><a class="b g" href="diagnose.cgi?${SIDQ}&action=newkey" onclick="return confirm('Neuen API-Key erzeugen? Der alte wird sofort ungültig.')">Neuen API-Key erzeugen</a></p>
<p class="s">Wenn du Hilfe brauchst: diese Seite komplett kopieren und weitergeben.</p>
<script>
// Aktion aus der Adresse entfernen, damit „Neu laden" sie nicht wiederholt
if (/action=/.test(location.search)) history.replaceState(null, '', location.pathname + '?${SIDQ}');
</script>
</body></html>
EOF
exit 0
