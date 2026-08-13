#!/bin/bash
set -e

# Package wl-joiner + luci app + init scripts into an OpenWRT .ipk file.
# Does NOT require the OpenWRT buildroot — creates the .ipk manually.
#
# Usage: ./package.sh [arch]
# arch: arm64 (default), arm, mipsle, mips, amd64
#
# The script expects the binary to already be built in dist/wl-joiner-<arch>.

ROOT="$(cd "$(dirname "$0")" && pwd)"
DIST="$ROOT/dist"
ARCH="${1:-arm64}"
VERSION="1.0.0"

BINARY="$DIST/wl-joiner-$ARCH"
if [ ! -f "$BINARY" ]; then
    echo "Binary not found: $BINARY"
    echo "Run ./build.sh $ARCH first"
    exit 1
fi

# Map Go arch to OpenWRT arch name
case "$ARCH" in
    arm64)  OPKG_ARCH="aarch64_generic" ;;
    arm)    OPKG_ARCH="arm_cortex-a7" ;;
    mipsle) OPKG_ARCH="mipsel_24kc" ;;
    mips)   OPKG_ARCH="mips_24kc" ;;
    amd64)  OPKG_ARCH="x86_64" ;;
    *)      OPKG_ARCH="$ARCH" ;;
esac

IPK_NAME="wlbypass_${VERSION}_${OPKG_ARCH}.ipk"
LUCI_IPK_NAME="luci-app-wlbypass_${VERSION}_all.ipk"

WORK="$DIST/ipk-work"
rm -rf "$WORK"

# ─── Main package: wlbypass ───────────────────────────────────
echo "=== Packaging wlbypass ($OPKG_ARCH) ==="

PKG="$WORK/wlbypass"
mkdir -p "$PKG/data/usr/bin"
mkdir -p "$PKG/data/etc/init.d"
mkdir -p "$PKG/data/etc/config"
mkdir -p "$PKG/control"

cp "$BINARY" "$PKG/data/usr/bin/wl-joiner"
chmod 755 "$PKG/data/usr/bin/wl-joiner"

cp "$ROOT/files/etc/init.d/wlbypass" "$PKG/data/etc/init.d/wlbypass"
chmod 755 "$PKG/data/etc/init.d/wlbypass"

cp "$ROOT/files/etc/config/wlbypass" "$PKG/data/etc/config/wlbypass"

# Control file
cat > "$PKG/control/control" << EOF
Package: wlbypass
Version: $VERSION
Architecture: $OPKG_ARCH
Maintainer: whitelist-bypass
Section: net
Priority: optional
Description: Whitelist Bypass VPN client - transparent proxy through Telemost
Depends: ip-full, kmod-tun, iptables-mod-conntrack-extra
Installed-Size: $(du -b "$BINARY" | cut -f1)
EOF

# Conffiles
cat > "$PKG/control/conffiles" << EOF
/etc/config/wlbypass
EOF

# Post-install script
cat > "$PKG/control/postinst" << 'EOF'
#!/bin/sh
[ -z "${IPKG_INSTROOT}" ] && {
    /etc/init.d/wlbypass enable 2>/dev/null || true
    echo "WL Bypass installed. Configure via LuCI: Services -> WL Bypass"
}
exit 0
EOF
chmod 755 "$PKG/control/postinst"

# Pre-remove script
cat > "$PKG/control/prerm" << 'EOF'
#!/bin/sh
[ -z "${IPKG_INSTROOT}" ] && {
    /etc/init.d/wlbypass stop 2>/dev/null || true
    /etc/init.d/wlbypass disable 2>/dev/null || true
}
exit 0
EOF
chmod 755 "$PKG/control/prerm"

echo "2.0" > "$PKG/debian-binary"

# Build tar archives
(cd "$PKG/data" && tar czf "$PKG/data.tar.gz" .)
(cd "$PKG/control" && tar czf "$PKG/control.tar.gz" .)

# Create .ipk (ar archive)
(cd "$PKG" && ar rc "$DIST/$IPK_NAME" debian-binary control.tar.gz data.tar.gz)
echo "  -> $DIST/$IPK_NAME"

# ─── LuCI package: luci-app-wlbypass ─────────────────────────
echo "=== Packaging luci-app-wlbypass (all) ==="

LUCI="$WORK/luci"
mkdir -p "$LUCI/data/usr/share/luci/menu.d"
mkdir -p "$LUCI/data/usr/share/rpcd/acl.d"
mkdir -p "$LUCI/data/www/luci-static/resources/view"
mkdir -p "$LUCI/control"

cp "$ROOT/luci-app-wlbypass/root/usr/share/luci/menu.d/luci-app-wlbypass.json" \
   "$LUCI/data/usr/share/luci/menu.d/"
cp "$ROOT/luci-app-wlbypass/root/usr/share/rpcd/acl.d/luci-app-wlbypass.json" \
   "$LUCI/data/usr/share/rpcd/acl.d/"
cp "$ROOT/luci-app-wlbypass/htdocs/luci-static/resources/view/wlbypass.js" \
   "$LUCI/data/www/luci-static/resources/view/"

cat > "$LUCI/control/control" << EOF
Package: luci-app-wlbypass
Version: $VERSION
Architecture: all
Maintainer: whitelist-bypass
Section: luci
Priority: optional
Description: LuCI interface for WL Bypass VPN client
Depends: luci-base, wlbypass
EOF

echo "2.0" > "$LUCI/debian-binary"
(cd "$LUCI/data" && tar czf "$LUCI/data.tar.gz" .)
(cd "$LUCI/control" && tar czf "$LUCI/control.tar.gz" .)
(cd "$LUCI" && ar rc "$DIST/$LUCI_IPK_NAME" debian-binary control.tar.gz data.tar.gz)
echo "  -> $DIST/$LUCI_IPK_NAME"

# Cleanup
rm -rf "$WORK"

echo ""
echo "=== Done ==="
echo ""
echo "Install on router:"
echo "  scp $DIST/$IPK_NAME $DIST/$LUCI_IPK_NAME root@<router>:/tmp/"
echo "  ssh root@<router>"
echo "  opkg install /tmp/$IPK_NAME"
echo "  opkg install /tmp/$LUCI_IPK_NAME"
echo "  # Then configure via LuCI: Services -> WL Bypass"
