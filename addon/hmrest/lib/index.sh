#!/bin/sh
# Startseite von HM-REST in der CCU-WebUI (Kachel in der Systemsteuerung)
. ${HMREST_ADDON:-/usr/local/addons/hmrest}/lib/common.sh
require_session

STATUS=$(${RC} status 2>&1)
page_head "HM-REST"
cat <<EOF
<div class="c">
<img src="icon.png" alt="" width="96" height="96">
<h1>HM-REST</h1>
<p>REST-API und Web-Oberfläche laufen auf Port 8099.</p>
<a class="b" id="go" href="#">HM-REST-Oberfläche öffnen</a>
<p class="s">Du wirst in der Oberfläche automatisch mit dem API-Key angemeldet.</p>
EOF
if [ "${STATUS}" = "running" ]; then
  echo "<p class='ok' id='state'>● Das Add-on läuft.</p>"
else
  echo "<p class='bad'>● Das Add-on läuft nicht.</p>"
fi
cat <<EOF
<p><a class="b g" href="diagnose.cgi?${SIDQ}">Diagnose / Add-on starten</a></p>
<p class="s" id="fw" style="display:none">🔴 Port 8099 ist von diesem Rechner aus nicht erreichbar.
Unter <i>Einstellungen → Systemsteuerung → Firewall</i> bei „Port-Freigabe" <code>8099</code> eintragen.</p>
</div>
<script>
var u = 'http://' + location.hostname + ':8099/';
// Sitzung nur im #-Teil weitergeben (wird nicht an Server/Proxys gesendet)
document.getElementById('go').href = u + '#sid=@${SID}@';
fetch(u + 'setup/status', {mode: 'no-cors'}).catch(function () {
  document.getElementById('fw').style.display = 'block';
});
</script>
</body></html>
EOF
exit 0
