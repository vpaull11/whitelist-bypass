//go:build !android

package androidbind

import "fmt"

func StartTun2Socks(fd, mtu, socksPort int, socksUser, socksPass string) error {
	return fmt.Errorf("tun2socks is only available on Android")
}

func StopTun2Socks() {}

type RoutedTunSession struct{}

func NewRoutedTunSession(fd, mtu, port int, user, pass, root string, p SocketProtector) (*RoutedTunSession, error) {
	return nil, fmt.Errorf("routed VPN is only available on Android")
}
func (*RoutedTunSession) Start() error              { return fmt.Errorf("routed VPN is only available on Android") }
func (*RoutedTunSession) SetGoogleDNS(enabled bool) {}
func (*RoutedTunSession) Stop()                     {}
