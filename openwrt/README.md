# WL Bypass — OpenWRT Plugin

Transparent VPN proxy for OpenWRT routers. Routes all LAN client traffic through a Telemost video call tunnel.

## How It Works

```
LAN Clients → br-lan → iptables mark → tun0 → tun2socks → SOCKS5 → WebRTC tunnel → Internet
```

The router's own traffic (including the joiner's connection to Telemost servers) goes directly through WAN — only LAN client traffic is tunneled.

## Prerequisites

- OpenWRT 19.07+ with LuCI
- `ip-full`, `kmod-tun`, `iptables-mod-conntrack-extra` packages
- A running creator server with an active Telemost conference link

## Quick Install

### 1. Build the binary

On your development machine (requires Go 1.21+):

```bash
cd openwrt
./build.sh arm64    # or: arm, mipsle, mips, amd64
./package.sh arm64
```

### 2. Install on the router

```bash
scp dist/wlbypass_1.0.0_aarch64_generic.ipk root@router:/tmp/
scp dist/luci-app-wlbypass_1.0.0_all.ipk root@router:/tmp/

ssh root@router
opkg install /tmp/wlbypass_1.0.0_aarch64_generic.ipk
opkg install /tmp/luci-app-wlbypass_1.0.0_all.ipk
```

### 3. Configure

Go to **LuCI → Services → WL Bypass**:

1. Paste the Telemost conference link
2. Enable the service
3. Click **Save & Apply**

### Manual Install (without .ipk)

```bash
# Copy binary
scp openwrt/dist/wl-joiner-arm64 root@router:/usr/bin/wl-joiner
ssh root@router chmod +x /usr/bin/wl-joiner

# Copy config files
scp openwrt/files/etc/config/wlbypass root@router:/etc/config/
scp openwrt/files/etc/init.d/wlbypass root@router:/etc/init.d/
ssh root@router chmod +x /etc/init.d/wlbypass

# Enable and start
ssh root@router
uci set wlbypass.main.link='https://telemost.yandex.ru/j/...'
uci set wlbypass.main.enabled='1'
uci commit wlbypass
/etc/init.d/wlbypass enable
/etc/init.d/wlbypass start
```

## Manual Usage (CLI)

```bash
# SOCKS5 proxy only (no transparent proxy)
wl-joiner --tm-link 'https://telemost.yandex.ru/j/12345' --socks-port 1080

# Transparent proxy (redirects LAN traffic through tunnel)
wl-joiner --tm-link 'https://telemost.yandex.ru/j/12345' --tun --lan-iface br-lan

# With debug logging
wl-joiner --tm-link 'https://telemost.yandex.ru/j/12345' --tun --debug
```

## Configuration Options

| UCI Option | Flag | Default | Description |
|------------|------|---------|-------------|
| `enabled` | — | `0` | Enable/disable the service |
| `link` | `--tm-link` | — | Telemost conference link (required) |
| `resources` | `--resources` | `moderate` | Memory mode: `moderate`, `default`, `unlimited` |
| `lan_iface` | `--lan-iface` | `br-lan` | LAN interface for traffic redirection |
| `dns` | `--dns` | `1.1.1.1,8.8.8.8` | DNS servers for tunneled clients |
| `debug` | `--debug` | `0` | Verbose debug logging |

## Architecture

The plugin uses **policy-based routing** to avoid disrupting the router's own connectivity:

1. `iptables -t mangle` marks new packets from `br-lan` with fwmark `0x1`
2. `ip rule` sends marked packets to routing table `100`
3. Table `100` has `default dev tun0`
4. `tun2socks` reads packets from `tun0` and proxies them through the SOCKS5 tunnel

This means the router's own traffic (NTP, DNS, package updates, and the joiner's WebRTC connection) all go through WAN normally.

## Supported Architectures

| Architecture | OpenWRT Target | Build Command |
|-------------|----------------|---------------|
| ARM64 / AArch64 | `aarch64_generic` | `./build.sh arm64` |
| ARM (v5) | `arm_cortex-a7` | `./build.sh arm` |
| MIPS LE | `mipsel_24kc` | `./build.sh mipsle` |
| MIPS BE | `mips_24kc` | `./build.sh mips` |
| x86_64 | `x86_64` | `./build.sh amd64` |
