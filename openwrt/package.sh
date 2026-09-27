#!/bin/bash
set -e

cd "$(dirname "$0")"
DIST="dist"
VERSION="1.0.0"
ARCH="arm64"
OPKG_ARCH="aarch64_generic"
BINARY="$DIST/wl-joiner-$ARCH"
WORK="$DIST/ipk-work"

if [ ! -f "$BINARY" ]; then
    echo "Binary not found: $BINARY"
    exit 1
fi

rm -rf "$WORK"

# === wlbypass package ===
echo "=== Packaging wlbypass ($OPKG_ARCH) ==="

PKG="$WORK/wlbypass"
mkdir -p "$PKG/data/usr/bin" "$PKG/data/etc/init.d" "$PKG/data/etc/config" "$PKG/control"

cp "$BINARY" "$PKG/data/usr/bin/wl-joiner"
chmod 755 "$PKG/data/usr/bin/wl-joiner"
cp files/etc/init.d/wlbypass "$PKG/data/etc/init.d/wlbypass"
chmod 755 "$PKG/data/etc/init.d/wlbypass"
cp files/etc/config/wlbypass "$PKG/data/etc/config/wlbypass"

INSTALLED_SIZE=$(wc -c < "$BINARY" | tr -d ' ')

printf 'Package: wlbypass\nVersion: %s\nArchitecture: %s\nMaintainer: whitelist-bypass\nSection: net\nPriority: optional\nDescription: Whitelist Bypass VPN client - transparent proxy through Telemost\nDepends: ip-full, kmod-tun, iptables-mod-conntrack-extra\nInstalled-Size: %s\n' \
    "$VERSION" "$OPKG_ARCH" "$INSTALLED_SIZE" > "$PKG/control/control"

echo "/etc/config/wlbypass" > "$PKG/control/conffiles"

printf '#!/bin/sh\n[ -z "${IPKG_INSTROOT}" ] && {\n    /etc/init.d/wlbypass enable 2>/dev/null || true\n}\nexit 0\n' > "$PKG/control/postinst"
chmod 755 "$PKG/control/postinst"

printf '#!/bin/sh\n[ -z "${IPKG_INSTROOT}" ] && {\n    /etc/init.d/wlbypass stop 2>/dev/null || true\n    /etc/init.d/wlbypass disable 2>/dev/null || true\n}\nexit 0\n' > "$PKG/control/prerm"
chmod 755 "$PKG/control/prerm"

echo "2.0" > "$PKG/debian-binary"
(cd "$PKG/data" && tar czf ../data.tar.gz .)
(cd "$PKG/control" && tar czf ../control.tar.gz .)

IPK_NAME="wlbypass_${VERSION}_${OPKG_ARCH}.ipk"
(cd "$PKG" && tar czf "../../$IPK_NAME" debian-binary control.tar.gz data.tar.gz)
echo "  -> $DIST/$IPK_NAME"

# === luci-app-wlbypass package ===
echo "=== Packaging luci-app-wlbypass (all) ==="

LUCI="$WORK/luci"
mkdir -p "$LUCI/data/usr/share/luci/menu.d"
mkdir -p "$LUCI/data/usr/share/rpcd/acl.d"
mkdir -p "$LUCI/data/www/luci-static/resources/view"
mkdir -p "$LUCI/control"

cp luci-app-wlbypass/root/usr/share/luci/menu.d/luci-app-wlbypass.json "$LUCI/data/usr/share/luci/menu.d/"
cp luci-app-wlbypass/root/usr/share/rpcd/acl.d/luci-app-wlbypass.json "$LUCI/data/usr/share/rpcd/acl.d/"
cp luci-app-wlbypass/htdocs/luci-static/resources/view/wlbypass.js "$LUCI/data/www/luci-static/resources/view/"

printf 'Package: luci-app-wlbypass\nVersion: %s\nArchitecture: all\nMaintainer: whitelist-bypass\nSection: luci\nPriority: optional\nDescription: LuCI interface for WL Bypass VPN client\nDepends: luci-base, wlbypass\n' \
    "$VERSION" > "$LUCI/control/control"

echo "2.0" > "$LUCI/debian-binary"
(cd "$LUCI/data" && tar czf ../data.tar.gz .)
(cd "$LUCI/control" && tar czf ../control.tar.gz .)

LUCI_IPK_NAME="luci-app-wlbypass_${VERSION}_all.ipk"
(cd "$LUCI" && tar czf "../../$LUCI_IPK_NAME" debian-binary control.tar.gz data.tar.gz)
echo "  -> $DIST/$LUCI_IPK_NAME"

rm -rf "$WORK"

echo ""
echo "=== Done ==="
ls -lh "$DIST"/*.ipk
echo ""
echo "Install on router:"
echo "  scp $DIST/$IPK_NAME $DIST/$LUCI_IPK_NAME root@<router>:/tmp/"
echo "  ssh root@<router> opkg install /tmp/$IPK_NAME /tmp/$LUCI_IPK_NAME"
