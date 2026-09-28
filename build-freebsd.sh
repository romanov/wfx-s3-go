#!/bin/sh
set -eu

if ! command -v go >/dev/null 2>&1; then
	echo "go was not found on PATH. Install Go 1.24 or newer." >&2
	exit 1
fi

go_version=$(go version)
case "$go_version" in
*" go1."[0-9]"."*|*" go1."[0-9][0-9]"."*) ;;
*)
	echo "Unable to determine Go version from: $go_version" >&2
	exit 1
	;;
esac

minor=$(printf '%s\n' "$go_version" | sed -n 's/.* go1\.\([0-9][0-9]*\).*/\1/p')
if [ -z "$minor" ] || [ "$minor" -lt 24 ]; then
	echo "Go 1.24 or newer is required; found $go_version" >&2
	exit 1
fi

export CGO_ENABLED=1
export GOOS=freebsd
export GOARCH=amd64

mkdir -p dist/freebsd-amd64
go mod download
go test ./...
go build -trimpath -buildmode=c-shared -o dist/freebsd-amd64/wfxs3.wfx ./cmd/wfxs3
cp -f wfxs3.ini.example dist/freebsd-amd64/wfxs3.ini.example

echo "Built dist/freebsd-amd64/wfxs3.wfx"
