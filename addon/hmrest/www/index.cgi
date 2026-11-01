#!/bin/tclsh
# Aufrufer fuer die Startseite (der CCU-Webserver startet .cgi mit tclsh)
if {[catch {exec /bin/sh /usr/local/addons/hmrest/lib/index.sh 2>@1} out]} {}
puts -nonewline $out
