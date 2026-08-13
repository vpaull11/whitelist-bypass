// Package main implements a Telemost joiner with transparent proxy for OpenWRT.
//
// It combines the headless Telemost joiner with tun2socks to create a
// transparent VPN gateway on an OpenWRT router. LAN client traffic is
// redirected through a TUN interface via policy-based routing (iptables
// mark + ip rule), while the joiner's own signaling/media traffic goes
// directly through WAN.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"whitelist-bypass/relay/common"
	"whitelist-bypass/relay/pion"
	joiner "whitelist-bypass/relay/pion/headless-joiner-common"
	"whitelist-bypass/relay/tunnel"
)

type cliStatusEmitter struct{}

func (cliStatusEmitter) EmitStatus(status string)   { log.Printf("[status] %s", status) }
func (cliStatusEmitter) EmitStatusError(msg string) { log.Printf("[status] ERROR: %s", msg) }

func resolveHostname(hostname string) (string, error) {
	if ip := net.ParseIP(hostname); ip != nil {
		return hostname, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", hostname); err == nil && len(ips) > 0 {
		return ips[0].String(), nil
	}
	if ips, err := net.DefaultResolver.LookupIP(ctx, "ip6", hostname); err == nil && len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", fmt.Errorf("no IPs for %s", hostname)
}

func main() {
	common.MaybePrintVersion()

	tmLink := flag.String("tm-link", "", "Telemost conference URI to join (required)")
	displayName := flag.String("name", "Router", "display name in the conference")
	socksPort := flag.Int("socks-port", 1080, "internal SOCKS5 port")
	socksUser := flag.String("socks-user", "", "SOCKS5 username (optional)")
	socksPass := flag.String("socks-pass", "", "SOCKS5 password (optional)")
	tunEnabled := flag.Bool("tun", false, "enable transparent proxy via TUN interface")
	tunNameFlag := flag.String("tun-name", defaultTunName, "TUN device name")
	lanIface := flag.String("lan-iface", defaultLANIface, "LAN interface for traffic redirection")
	dnsFlag := flag.String("dns", "1.1.1.1,8.8.8.8", "DNS servers (comma-separated)")
	resources := flag.String("resources", "moderate", "resource mode: moderate, default, unlimited")
	vp8FPS := flag.Int("vp8-fps", 24, "VP8 frame rate")
	vp8Batch := flag.Int("vp8-batch", 30, "VP8 batch multiplier")
	debugFlag := flag.Bool("debug", false, "verbose debug logging")
	flag.Parse()
	common.Debug = *debugFlag

	if *tmLink == "" {
		log.Fatal("--tm-link is required")
	}

	var memLimit int64
	switch *resources {
	case "moderate":
		memLimit = 64 << 20
	case "default":
		memLimit = 128 << 20
	case "unlimited":
		memLimit = 256 << 20
	default:
		log.Fatalf("[config] unknown resources mode: %s", *resources)
	}
	if memLimit > 0 {
		debug.SetMemoryLimit(memLimit)
	}
	log.Printf("[config] resources=%s tun=%v lan=%s", *resources, *tunEnabled, *lanIface)

	var (
		routing   *Routing
		routingMu sync.Mutex
	)

	inner := joiner.NewTelemostHeadlessJoiner(
		log.Printf,
		resolveHostname,
		cliStatusEmitter{},
		nil,
		pion.AddTunnelTracks,
		pion.ReadTrack,
	)

	var activeBridge *tunnel.RelayBridge
	inner.OnConnected = func(tun tunnel.DataTunnel) {
		readBuf := common.VP8BufSize
		if _, ok := tun.(*tunnel.DCTunnel); ok {
			readBuf = common.DCBufSize
		}
		bridge := tunnel.NewRelayBridgeWithAuth(tun, "joiner", readBuf, log.Printf, *socksUser, *socksPass)
		bridge.SetOnConfigAck(inner.MarkConfigAcked)
		bridge.MarkReady()
		activeBridge = bridge

		socksAddr := fmt.Sprintf("127.0.0.1:%d", *socksPort)
		go func() {
			if err := bridge.ListenSOCKS(socksAddr); err != nil {
				log.Printf("[socks] listen: %v", err)
			}
		}()
		log.Printf("[tunnel] CONNECTED — socks5 -> %s", socksAddr)
		fmt.Println("")
		fmt.Println("  TUNNEL CONNECTED")
		fmt.Printf("  socks5 -> %s\n", socksAddr)
		fmt.Println("")

		if *tunEnabled {
			routingMu.Lock()
			defer routingMu.Unlock()

			// Cleanup previous routing if reconnecting
			if routing != nil {
				routing.Stop()
			}

			dns := strings.Split(*dnsFlag, ",")
			r, err := NewRouting(RoutingConfig{
				TunName:   *tunNameFlag,
				LANIface:  *lanIface,
				SocksPort: *socksPort,
				DNSServers: dns,
				LogFn:     log.Printf,
			})
			if err != nil {
				log.Printf("[routing] setup failed: %v", err)
				return
			}
			if err := r.Start(); err != nil {
				log.Printf("[routing] start failed: %v", err)
				return
			}
			routing = r
			log.Printf("[routing] transparent proxy active on %s", *tunNameFlag)
			fmt.Println("  TRANSPARENT PROXY ACTIVE")
		}
	}

	params, _ := json.Marshal(struct {
		JoinLink    string `json:"joinLink"`
		DisplayName string `json:"displayName"`
		VP8FPS      int    `json:"vp8Fps"`
		VP8Batch    int    `json:"vp8Batch"`
	}{
		JoinLink:    strings.TrimSpace(*tmLink),
		DisplayName: *displayName,
		VP8FPS:      *vp8FPS,
		VP8Batch:    *vp8Batch,
	})

	go inner.RunWithParams(string(params))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	log.Println("[main] shutting down")

	routingMu.Lock()
	if routing != nil {
		routing.Stop()
		routing = nil
	}
	routingMu.Unlock()

	if activeBridge != nil {
		activeBridge.Reset()
	}
	inner.Close()
}
