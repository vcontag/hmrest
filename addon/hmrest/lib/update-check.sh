#!/bin/sh
# Update-Prüfung für HM-REST – aufgerufen von www/update-check.cgi
#
# Ohne Parameter: gibt die neueste veröffentlichte Version als Text aus (z.B. "1.6.0"), sonst "n/a".
#                 So erwartet es die CCU-WebUI (Zeile "Update:" in "rc.d/hmrest info").
# ?cmd=download:  leitet auf die Release-Seite bei GitHub weiter.
# ?cmd=json:      {"installed":"…","latest":"…","url":"…"} für die Kachel-Seite.

REPO=${HMREST_REPO:-vcontag/hmrest}
ADDON=${HMREST_ADDON:-/usr/local/addons/hmrest}
CACHE=${HMREST_UPDATE_CACHE:-/tmp/hmrest-latest-version}
API="https://api.github.com/repos/${REPO}/releases/latest"
PAGE="https://github.com/${REPO}/releases/latest"
MAXAGE=3600

CMD=$(echo "${QUERY_STRING}" | sed -n -r 's/(^|.*&)cmd=([a-z]*).*/\2/p')

if [ "${CMD}" = "download" ]; then
  echo "Status: 302 Found"
  echo "Location: ${PAGE}"
  echo "Content-Type: text/html; charset=utf-8"
  echo ""
  echo "<html><head><meta http-equiv='refresh' content='0; url=${PAGE}'></head><body><a href='${PAGE}'>${PAGE}</a></body></html>"
  exit 0
fi

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -m 10 -H "Accept: application/vnd.github+json" -A "hmrest-update-check" "$1" 2>/dev/null && return 0
  fi
  wget -q -T 10 -U "hmrest-update-check" -O - "$1" 2>/dev/null && return 0
  wget -q -T 10 -U "hmrest-update-check" --no-check-certificate -O - "$1" 2>/dev/null
}

latest() {
  # Zwischenspeicher (1 Stunde) – GitHub erlaubt ohne Anmeldung nur 60 Abfragen pro Stunde
  if [ -s "${CACHE}" ]; then
    age=$(( $(date +%s) - $(date -r "${CACHE}" +%s 2>/dev/null || echo 0) ))
    if [ "${age}" -ge 0 ] && [ "${age}" -lt ${MAXAGE} ]; then
      cat "${CACHE}"
      return
    fi
  fi
  v=$(fetch "${API}" | sed -n 's/.*"tag_name" *: *"v\{0,1\}\([0-9][0-9A-Za-z.+-]*\)".*/\1/p' | head -n 1)
  if [ -n "${v}" ]; then
    echo "${v}" > "${CACHE}" 2>/dev/null
    echo "${v}"
  elif [ -s "${CACHE}" ]; then
    cat "${CACHE}"   # GitHub gerade nicht erreichbar: letzten bekannten Stand nehmen
  else
    echo "n/a"
  fi
}

V=$(latest)
if [ "${CMD}" = "json" ]; then
  INST=$(cat "${ADDON}/VERSION" 2>/dev/null)
  echo "Content-Type: application/json; charset=utf-8"
  echo "Cache-Control: no-store"
  echo ""
  printf '{"installed":"%s","latest":"%s","url":"%s"}\n' "${INST}" "${V}" "${PAGE}"
else
  echo "Content-Type: text/plain; charset=utf-8"
  echo ""
  echo "${V}"
fi
exit 0
