#!/bin/sh
# Baut die Programme für alle CCU-Plattformen und packt das Add-on.
set -e
cd "$(dirname "$0")"
VERSION=$(grep 'var version' main.go | sed 's/.*"\(.*\)".*/\1/')
OUT=dist/hmrest-${VERSION}
rm -rf dist && mkdir -p ${OUT}/bin ${OUT}/rc.d ${OUT}/hmrest/etc ${OUT}/hmrest/www ${OUT}/hmrest/lib

build() { CGO_ENABLED=0 GOOS=linux GOARCH=$1 GOARM=$2 go build -trimpath -ldflags="-s -w" -o ${OUT}/bin/hmrest-$3 . ; }
build arm 5 arm        # CCU2, CCU3, RaspberryMatic 32 Bit, Tinkerboard (GOARM=5 läuft überall)
build arm64 "" arm64   # RaspberryMatic 64 Bit (RPi 3/4/5)
build amd64 "" amd64   # RaspberryMatic OVA / x86
build 386 "" 386

cp addon/update_script ${OUT}/update_script
cp addon/rc.d/hmrest ${OUT}/rc.d/hmrest
cp addon/hmrest/www/* ${OUT}/hmrest/www/
cp addon/hmrest/lib/* ${OUT}/hmrest/lib/
cp addon/hmrest/etc/config.default.json addon/hmrest/etc/hm_addons.cfg ${OUT}/hmrest/etc/
echo ${VERSION} > ${OUT}/VERSION
chmod 755 ${OUT}/update_script ${OUT}/rc.d/hmrest ${OUT}/bin/* ${OUT}/hmrest/www/*.cgi ${OUT}/hmrest/lib/*.sh

( cd ${OUT} && tar --owner=0 --group=0 -czf ../hmrest-${VERSION}.tar.gz * )
ls -la dist
