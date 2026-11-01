#!/bin/tclsh
# Update-Prüfung (von der CCU-WebUI unter Zusatzsoftware aufgerufen)
if {[catch {exec /bin/sh /usr/local/addons/hmrest/lib/update-check.sh 2>@1} out]} {}
puts -nonewline $out
