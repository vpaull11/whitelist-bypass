#!/bin/bash
set -e

# Build wl-joiner for OpenWRT targets.
# Usage: ./build.sh [arch]
# arch: arm64 (default), arm, mipsle, mips, amd64

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/headless/openwrt-joiner"
DIST="$ROOT/openwrt/dist"
mkdir -p "$DIST"

ARCH="${1:-arm64}"

case "$ARCH" in
    arm64)
        export GOOS=linux GOARCH=arm64
        ;;
    arm)
        export GOOS=linux GOARCH=arm GOARM=5
        ;;
    mipsle)
        export GOOS=linux GOARCH=mipsle GOMIPS=softfloat
        ;;
    mips)
        export GOOS=linux GOARCH=mips GOMIPS=softfloat
        ;;
    amd64)
        export GOOS=linux GOARCH=amd64
        ;;
    *)
        echo "Unknown architecture: $ARCH"
        echo "Supported: arm64, arm, mipsle, mips, amd64"
        exit 1
        ;;
esac

export CGO_ENABLED=0
OUTPUT="$DIST/wl-joiner-$ARCH"

echo "=== Building wl-joiner for $GOOS/$GOARCH ==="
go -C "$SRC" build -trimpath -ldflags="-s -w" -o "$OUTPUT" .
echo "  -> $OUTPUT ($(du -h "$OUTPUT" | cut -f1))"

echo ""
echo "=== Done ==="
echo "Binary: $OUTPUT"
echo ""
echo "To install on the router:"
echo "  scp $OUTPUT root@<router>:/usr/bin/wl-joiner"
echo "  ssh root@<router> chmod +x /usr/bin/wl-joiner"
