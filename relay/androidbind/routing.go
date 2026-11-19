package androidbind

import (
	"encoding/json"
	"whitelist-bypass/relay/routing"
)

// SocketProtector is implemented by Android VpnService.protect. DIRECT sockets
// must be protected before connect; failure is fatal for that flow.
type SocketProtector interface{ Protect(fd int) bool }
type RoutingProgress interface {
	OnProgress(stage string, bytes int64)
	IsCancelled() bool
}

func PreviewRoutingProfile(input string) (string, error) { return routing.Preview(input) }
func PrepareRoutingProfile(root, input string, progress RoutingProgress, protector SocketProtector) (string, error) {
	return routing.Prepare(root, input, progress, protector)
}
func RoutingProfileInfo(root string) (string, error) {
	s, err := routing.Load(root)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(s.Profile)
	return string(b), err
}
func DeleteRoutingProfile(root string) error { return routing.Remove(root) }
func RoutingDNSAddress() string              { return routing.DNSAddress }
