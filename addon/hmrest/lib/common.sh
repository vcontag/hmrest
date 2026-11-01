#!/bin/sh
# Gemeinsame Funktionen der CCU-Seiten von HM-REST
ADDON=${HMREST_ADDON:-/usr/local/addons/hmrest}
RC=${HMREST_RC:-/usr/local/etc/config/rc.d/hmrest}
LOG=${HMREST_LOG:-/var/log/hmrest.log}
CONFIG=${ADDON}/etc/config.json

esc() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

# Parameter aus QUERY_STRING lesen (nur [A-Za-z0-9])
qparam() { echo "${QUERY_STRING}" | sed -n -r "s/(^|.*&)$1=([A-Za-z0-9]*).*/\2/p"; }

# Sitzungs-ID der WebUI (sid=@XXXXXXXXXX@, @ evtl. als %40)
SID=$(echo "${QUERY_STRING}" | sed -n -r 's/(^|.*&)sid=(%40|@)?([0-9A-Za-z]{10}).*/\3/p')

page_head() {
  echo "Content-Type: text/html; charset=utf-8"
  echo "Cache-Control: no-store"
  echo ""
  cat <<EOF
<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>$1</title><style>
body{font:14px/1.45 system-ui,sans-serif;max-width:820px;margin:20px auto;padding:0 14px;color:#222}
pre{background:#f1f3f6;padding:10px;border-radius:6px;white-space:pre-wrap;word-break:break-all;font-size:12px}
a.b{display:inline-block;background:#1f6feb;color:#fff;padding:9px 14px;border-radius:7px;text-decoration:none;margin:4px 6px 4px 0}
a.g{background:#e6e9ee;color:#222}.ok{color:#1a7f37;font-weight:600}.bad{color:#cf222e;font-weight:600}
.c{text-align:center} .s{color:#666;font-size:13px} code{background:#eef2f7;padding:1px 5px;border-radius:4px}
</style></head><body>
EOF
}

# Bricht mit einer Hinweisseite ab, wenn keine gültige WebUI-Sitzung vorliegt.
require_session() {
  if [ -n "${SID}" ] && ${ADDON}/bin/hmrest -config ${CONFIG} -checksession "@${SID}@" >/dev/null 2>&1; then
    return 0
  fi
  page_head "HM-REST – Anmeldung erforderlich"
  cat <<'EOF'
<script>
// Aus der WebUI geöffnet, aber ohne/mit alter sid? Sitzung aus dem WebUI-Fenster übernehmen.
(function () {
  var w = null;
  try { if (window.opener && window.opener.SessionId) w = window.opener; } catch (e) {}
  try { if (!w && window.parent !== window && window.parent.SessionId) w = window.parent; } catch (e) {}
  if (!w) return;
  var s = String(w.SessionId).replace(/@/g, '');
  if (/^[0-9A-Za-z]{10}$/.test(s) && location.search.indexOf(s) < 0) {
    location.replace(location.pathname + '?sid=@' + s + '@');
  }
})();
</script>
<div class="c">
<h2>Anmeldung erforderlich</h2>
<p>Diese Seite ist nur mit einer gültigen Anmeldung an der CCU erreichbar.<br>
Bitte in der WebUI anmelden und die Seite über <b>Einstellungen → Systemsteuerung → HM-REST</b> öffnen.</p>
<a class="b" href="/">Zur Anmeldung</a>
</div>
<p class="s c">Die Seite muss aus der WebUI heraus geöffnet werden, ein Lesezeichen funktioniert nicht.</p>
</body></html>
EOF
  exit 0
}

# Link-Parameter mit Sitzung
SIDQ="sid=@${SID}@"
