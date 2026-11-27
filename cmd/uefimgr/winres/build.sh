#!/bin/sh
# Renders the icon from its SVG sources with headless Chrome and builds the
# Windows resource objects that go build links into the executable.
# The small sizes have their own pixel-aligned drawings.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
chrome=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

render() { # render SVG SIZE
  printf '<!doctype html><body style="margin:0"><img src="file://%s" width="%s" height="%s" style="display:block">' \
    "$here/$1" "$2" "$2" > "$tmp/page.html"
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
    --default-background-color=00000000 --allow-file-access-from-files \
    --window-size="$2,$2" --screenshot="$here/icon_$2.png" "file://$tmp/page.html" >/dev/null 2>&1
}

render icon-16.svg 16
render icon-32.svg 32
for s in 48 64 256; do render icon.svg "$s"; done

cd "$here/.."
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --out rsrc --arch amd64,arm64
