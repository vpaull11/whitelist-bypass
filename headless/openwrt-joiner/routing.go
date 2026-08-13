//go:build linux

package main

import (
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

const (
	defaultTunName  = "tun0"
	defaultLANIface = "br-lan"
	tunIP           = "198.18.0.1"
	tunCIDR         = "198.18.0.1/15"
	tunMTU          = 1500
	rtTable         = "100"
	fwMark          = "0x1"
)

// RoutingConfig holds the configuration for OpenWRT transparent proxy routing.
type RoutingConfig struct {
	TunName    string
	LANIface   string
	SocksPort  int
	SocksUser  string
	SocksPass  string
	DNSServers []string
	LogFn      func(string, ...any)
}

// Routing manages the TUN interface, tun2socks engine, and OpenWRT
// policy-based routing rules. It redirects traffic from LAN clients
// through the tunnel while keeping the router's own WAN traffic intact.
//
// The approach uses iptables fwmark + ip rule + ip route table:
//
//	1. iptables marks packets arriving on br-lan (LAN interface)
//	2. ip rule sends marked packets to routing table 100
//	3. Table 100 has default route via tun0
//	4. tun2socks reads packets from tun0 and sends them through SOCKS5
//
// This avoids touching the router's main routing table, so the joiner's
// own traffic (to Telemost SFU servers) goes directly through WAN.
type Routing struct {
	cfg     RoutingConfig
	log     func(string, ...any)
	started bool
}

// NewRouting creates a new Routing instance.
func NewRouting(cfg RoutingConfig) (*Routing, error) {
	if cfg.TunName == "" {
		cfg.TunName = defaultTunName
	}
	if cfg.LANIface == "" {
		cfg.LANIface = defaultLANIface
	}
	if cfg.SocksPort <= 0 {
		return nil, fmt.Errorf("routing: invalid socks port %d", cfg.SocksPort)
	}
	if cfg.LogFn == nil {
		cfg.LogFn = log.Printf
	}
	return &Routing{cfg: cfg, log: cfg.LogFn}, nil
}

// Start creates the TUN interface, starts tun2socks, and sets up
// policy-based routing to redirect LAN traffic through the tunnel.
func (r *Routing) Start() error {
	if r.started {
		return fmt.Errorf("routing: already started")
	}

	// 1. Start tun2socks engine (creates TUN device)
	proxy := fmt.Sprintf("socks5://127.0.0.1:%d", r.cfg.SocksPort)
	if r.cfg.SocksUser != "" {
		proxy = fmt.Sprintf("socks5://%s:%s@127.0.0.1:%d",
			r.cfg.SocksUser, r.cfg.SocksPass, r.cfg.SocksPort)
	}

	key := &engine.Key{
		Proxy:  proxy,
		Device: "tun://" + r.cfg.TunName,
		MTU:    tunMTU,
	}
	r.log("[routing] starting tun2socks: dev=%s mtu=%d proxy=%s", r.cfg.TunName, tunMTU, proxy)
	engine.Insert(key)
	engine.Start()

	// 2. Configure TUN interface
	if out, err := runCmd("ip", "addr", "add", tunCIDR, "dev", r.cfg.TunName); err != nil {
		engine.Stop()
		return fmt.Errorf("routing: ip addr add: %w (%s)", err, out)
	}
	if out, err := runCmd("ip", "link", "set", r.cfg.TunName, "mtu", strconv.Itoa(tunMTU)); err != nil {
		r.log("[routing] set mtu failed (continuing): %v (%s)", err, out)
	}
	if out, err := runCmd("ip", "link", "set", r.cfg.TunName, "up"); err != nil {
		engine.Stop()
		return fmt.Errorf("routing: ip link set up: %w (%s)", err, out)
	}

	// 3. Create routing table for tunneled traffic
	if out, err := runCmd("ip", "route", "add", "default", "dev", r.cfg.TunName, "table", rtTable); err != nil {
		r.log("[routing] add default route to table %s: %v (%s)", rtTable, err, out)
	}

	// 4. Add DNS routes to tunnel table
	for _, dns := range r.cfg.DNSServers {
		dns = strings.TrimSpace(dns)
		if dns == "" {
			continue
		}
		if out, err := runCmd("ip", "route", "add", dns+"/32", "dev", r.cfg.TunName, "table", rtTable); err != nil {
			r.log("[routing] add dns route %s: %v (%s)", dns, err, out)
		}
	}

	// 5. ip rule: send marked packets to our routing table
	if out, err := runCmd("ip", "rule", "add", "fwmark", fwMark, "table", rtTable, "priority", "100"); err != nil {
		r.log("[routing] ip rule add: %v (%s)", err, out)
	}

	// 6. iptables: mark packets from LAN interface (forwarded traffic)
	//    -m conntrack --ctstate NEW ensures only new connections are marked,
	//    existing connections (including joiner's own) keep their route.
	if out, err := runCmd("iptables", "-t", "mangle", "-A", "PREROUTING",
		"-i", r.cfg.LANIface,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "MARK", "--set-mark", fwMark); err != nil {
		r.log("[routing] iptables mark: %v (%s)", err, out)
	}

	// 7. Redirect DNS from LAN to our DNS servers (optional, improves DNS for clients)
	if len(r.cfg.DNSServers) > 0 {
		dns := strings.TrimSpace(r.cfg.DNSServers[0])
		if dns != "" {
			if out, err := runCmd("iptables", "-t", "nat", "-A", "PREROUTING",
				"-i", r.cfg.LANIface, "-p", "udp", "--dport", "53",
				"-j", "DNAT", "--to-destination", dns+":53"); err != nil {
				r.log("[routing] dns redirect: %v (%s)", err, out)
			}
			if out, err := runCmd("iptables", "-t", "nat", "-A", "PREROUTING",
				"-i", r.cfg.LANIface, "-p", "tcp", "--dport", "53",
				"-j", "DNAT", "--to-destination", dns+":53"); err != nil {
				r.log("[routing] dns redirect tcp: %v (%s)", err, out)
			}
		}
	}

	r.started = true
	r.log("[routing] transparent proxy active: %s traffic -> %s -> socks5://127.0.0.1:%d",
		r.cfg.LANIface, r.cfg.TunName, r.cfg.SocksPort)
	return nil
}

// Stop removes routing rules, iptables entries, and shuts down tun2socks.
func (r *Routing) Stop() {
	if !r.started {
		return
	}
	r.started = false

	// Remove iptables rules
	runCmd("iptables", "-t", "mangle", "-D", "PREROUTING",
		"-i", r.cfg.LANIface,
		"-m", "conntrack", "--ctstate", "NEW",
		"-j", "MARK", "--set-mark", fwMark)

	// Remove DNS redirect
	if len(r.cfg.DNSServers) > 0 {
		dns := strings.TrimSpace(r.cfg.DNSServers[0])
		if dns != "" {
			runCmd("iptables", "-t", "nat", "-D", "PREROUTING",
				"-i", r.cfg.LANIface, "-p", "udp", "--dport", "53",
				"-j", "DNAT", "--to-destination", dns+":53")
			runCmd("iptables", "-t", "nat", "-D", "PREROUTING",
				"-i", r.cfg.LANIface, "-p", "tcp", "--dport", "53",
				"-j", "DNAT", "--to-destination", dns+":53")
		}
	}

	// Remove ip rule
	runCmd("ip", "rule", "del", "fwmark", fwMark, "table", rtTable)

	// Remove routes from table
	runCmd("ip", "route", "flush", "table", rtTable)

	// Stop tun2socks (removes TUN interface)
	engine.Stop()

	r.log("[routing] transparent proxy stopped")
}

func runCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}
