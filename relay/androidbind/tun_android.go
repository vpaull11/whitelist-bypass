//go:build android

package androidbind

/*
#include <stdint.h>

void disable_fdsan() {
#ifdef __ANDROID__
    extern void android_fdsan_set_error_level(uint32_t) __attribute__((weak));
    if (android_fdsan_set_error_level) {
        android_fdsan_set_error_level(0);
    }
#endif
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	tun "github.com/xjasonlyu/tun2socks/v2/tunnel"
	"whitelist-bypass/relay/routing"

	"whitelist-bypass/relay/common"
)

var (
	tunReady     sync.WaitGroup
	tunMu        sync.Mutex
	activeRouter *routing.Router
	activeOwner  *RoutedTunSession
	engineActive bool
	activeFilter *v6Filter
)

func StartTun2Socks(fd, mtu, socksPort int, socksUser, socksPass string) error {
	tunMu.Lock()
	defer tunMu.Unlock()
	if engineActive {
		os.NewFile(uintptr(fd), "rejected-vpntun").Close()
		return errors.New("VPN engine is already running")
	}
	var proxy string
	if socksUser != "" {
		proxy = fmt.Sprintf("socks5://%s:%s@%s:%d", socksUser, socksPass, common.SocksLocalhostIP, socksPort)
	} else {
		proxy = fmt.Sprintf("socks5://%s:%d", common.SocksLocalhostIP, socksPort)
	}
	logMsg("tun2socks: starting fd=%d mtu=%d proxy=%s", fd, mtu, proxy)
	os.Setenv("TUN2SOCKS_LOG_LEVEL", "info")
	tunReady.Add(1)
	defer tunReady.Done()

	deviceFd := fd
	filter, stackFd, err := startV6Filter(fd, mtu)
	if err != nil {
		logMsg("tun2socks: v6 filter setup failed: %v, capturing IPv4 only", err)
	} else {
		activeFilter = filter
		deviceFd = stackFd
		logMsg("tun2socks: v6 reject filter active")
	}

	key := &engine.Key{
		Proxy:  proxy,
		Device: fmt.Sprintf("fd://%d", deviceFd),
		MTU:    mtu,
	}
	engine.Insert(key)
	engine.Start()
	engineActive = true
	logMsg("tun2socks: running")
	return nil
}

func StopTun2Socks() {
	tunMu.Lock()
	defer tunMu.Unlock()
	stopEngine()
}

type stoppedDialer struct{}

func (stoppedDialer) DialContext(context.Context, *M.Metadata) (net.Conn, error) {
	return nil, net.ErrClosed
}
func (stoppedDialer) DialUDP(*M.Metadata) (net.PacketConn, error) { return nil, net.ErrClosed }

func stopEngine() {
	// Release Java callbacks held by the global dialer without allowing queued
	// flows to fall back to DIRECT while the old netstack shuts down.
	tun.T().SetDialer(stoppedDialer{})
	if activeRouter != nil {
		activeRouter.Close()
		activeRouter = nil
	}
	activeOwner = nil
	if !engineActive {
		return
	}
	engineActive = false
	tunReady.Wait()
	C.disable_fdsan()
	engine.Stop()
	if activeFilter != nil {
		activeFilter.stop()
		activeFilter = nil
	}
	logMsg("tun2socks: stopped")
}

// RoutedTunSession owns the detached fd immediately. Stop before Start is safe,
// and an old session cannot stop a newer netstack generation.
type RoutedTunSession struct {
	mu               sync.Mutex
	fd, mtu, port    int
	user, pass, root string
	protector        SocketProtector
	stopped, started bool
	googleDNS        bool
}

func NewRoutedTunSession(fd, mtu, port int, user, pass, root string, p SocketProtector) (*RoutedTunSession, error) {
	if fd < 0 || mtu < 1280 || port < 1 || port > 65535 || p == nil {
		return nil, errors.New("invalid routed VPN session parameters")
	}
	return &RoutedTunSession{fd: fd, mtu: mtu, port: port, user: user, pass: pass, root: root, protector: p}, nil
}

// SetGoogleDNS selects tunnel-only Google DoH before Start.
func (s *RoutedTunSession) SetGoogleDNS(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started && !s.stopped {
		s.googleDNS = enabled
	}
}
func (s *RoutedTunSession) Start() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("VPN session already stopped")
	}
	if s.started {
		return nil
	}
	tunMu.Lock()
	defer tunMu.Unlock()
	defer func() {
		if err != nil {
			s.stopped = true
			s.protector = nil
			if s.fd >= 0 {
				os.NewFile(uintptr(s.fd), "failed-vpntun").Close()
				s.fd = -1
			}
		}
	}()
	if engineActive {
		return errors.New("VPN engine is already running")
	}
	snapshot, err := routing.Load(s.root)
	if err != nil {
		return err
	}
	outbound, err := proxy.NewSocks5(fmt.Sprintf("127.0.0.1:%d", s.port), s.user, s.pass)
	if err != nil {
		return err
	}
	router, err := routing.NewRouter(snapshot, s.protector, outbound, s.googleDNS)
	if err != nil {
		return err
	}
	gate := make(chan struct{})
	filter, stackFD, err := startV6FilterGated(s.fd, s.mtu, gate)
	if err != nil {
		router.Close()
		return err
	}
	// From here the filter owns fd; no ungated fallback is permitted.
	s.fd = -1
	proxyURL := &url.URL{Scheme: "socks5", Host: fmt.Sprintf("127.0.0.1:%d", s.port)}
	if s.user != "" {
		proxyURL.User = url.UserPassword(s.user, s.pass)
	}
	engine.Insert(&engine.Key{Proxy: proxyURL.String(), Device: fmt.Sprintf("fd://%d", stackFD), MTU: s.mtu})
	engine.Start()
	tun.T().SetDialer(router)
	activeFilter = filter
	activeRouter = router
	activeOwner = s
	engineActive = true
	s.started = true
	close(gate)
	logMsg("tun2socks: service routing enabled (DIRECT/PROXY/BLOCK, no FakeDNS)")
	return nil
}
func (s *RoutedTunSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		s.protector = nil
		return
	}
	s.stopped = true
	if s.fd >= 0 {
		os.NewFile(uintptr(s.fd), "stopped-vpntun").Close()
		s.fd = -1
	}
	tunMu.Lock()
	defer tunMu.Unlock()
	if activeOwner == s {
		stopEngine()
	}
	s.protector = nil
}
