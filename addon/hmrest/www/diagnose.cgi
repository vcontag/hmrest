#!/bin/tclsh
# Aufrufer fuer die Diagnose-Seite (der CCU-Webserver startet .cgi mit tclsh)
if {[catch {exec /bin/sh /usr/local/addons/hmrest/lib/diagnose.sh 2>@1} out]} {}
puts -nonewline $out
