package routing

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/net/dns/dnsmessage"
)

func TestGoogleDNSKeepsServiceRoutes(t *testing.T) {
	p := testProfile()
	p.DirectSites = []string{"domain:direct.example"}
	p.ProxySites = []string{"domain:proxy.example"}
	p.BlockSites = []string{"domain:block.example"}
	p.ProxyIP = []string{"5.1.2.0/24"}
	var protects, calls atomic.Int32
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { protects.Add(1); return false }), &fakeProxy{fail: true}, true)
	defer d.Close()
	if d.direct != nil {
		t.Fatal("Google policy created direct DNS client")
	}
	mock := mockDoH(&calls, [4]byte{9, 9, 9, 9})
	d.remote = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://dns.google/dns-query" {
			t.Errorf("not Google DoH: %s", req.URL)
		}
		return mock.Transport.RoundTrip(req)
	})}
	for _, tc := range []struct {
		name string
		want Action
	}{{"direct.example", Direct}, {"proxy.example", Proxy}, {"unknown.example", Direct}} {
		q := query(tc.name)
		if _, err := validateAnswer(q, d.Query(context.Background(), q)); err != nil {
			t.Fatal(err)
		}
		if a, ok := d.Cached(netip.MustParseAddr("9.9.9.9")); !ok || a != tc.want {
			t.Fatalf("%s cached %s, want %s", tc.name, a, tc.want)
		}
		// Isolate each domain from the intentionally conservative shared-IP policy.
		d.mu.Lock()
		clear(d.cache)
		d.mu.Unlock()
	}
	if calls.Load() != 3 {
		t.Fatal("extra lookups", calls.Load())
	}
	var blocked dnsmessage.Message
	blocked.Unpack(d.Query(context.Background(), query("block.example")))
	if blocked.RCode != dnsmessage.RCodeRefused || calls.Load() != 3 {
		t.Fatal("BLOCK queried Google")
	}
	d.remote = mockDoH(&calls, [4]byte{5, 1, 2, 3})
	d.Query(context.Background(), query("ip-rule.example"))
	if a, _ := d.Cached(netip.MustParseAddr("5.1.2.3")); a != Proxy || calls.Load() != 4 {
		t.Fatal("IPIfNonMatch decision lost or resolved twice")
	}
	d.remote = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	var failed dnsmessage.Message
	failed.Unpack(d.Query(context.Background(), query("failure.example")))
	if failed.RCode != dnsmessage.RCodeServerFailure || protects.Load() != 0 {
		t.Fatal("Google DNS failed open")
	}
}

type googleOfflineProxy struct{ destinations []netip.Addr }

func (p *googleOfflineProxy) DialContext(_ context.Context, m *M.Metadata) (net.Conn, error) {
	p.destinations = append(p.destinations, m.DstIP)
	if m.DstPort != 443 {
		return nil, errors.New("wrong port")
	}
	return nil, errors.New("offline")
}
func (*googleOfflineProxy) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, errors.New("unexpected UDP")
}
func TestGoogleDNSBootstrapOnlyThroughTunnel(t *testing.T) {
	p := testProfile()
	p.DnsHosts["dns.google"] = "9.9.9.9"
	out := &googleOfflineProxy{}
	var protects atomic.Int32
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { protects.Add(1); return false }), out, true)
	defer d.Close()
	tr := d.remote.Transport.(*http.Transport)
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification disabled")
	}
	var reply dnsmessage.Message
	reply.Unpack(d.Query(context.Background(), query("direct.example")))
	if reply.RCode != dnsmessage.RCodeServerFailure || protects.Load() != 0 {
		t.Fatal("DNS not fail closed")
	}
	if len(out.destinations) != 2 || out.destinations[0].String() != "8.8.8.8" || out.destinations[1].String() != "8.8.4.4" {
		t.Fatal("wrong tunneled bootstrap", out.destinations)
	}
}

func TestBundledDefaultProfile(t *testing.T) {
	b, err := os.ReadFile("../../android-app/app/src/main/assets/default-routing.txt")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "RU-SPLIT" || p.FakeDNS || p.GlobalProxy || p.RouteOrder != "block-proxy-direct" || len(p.ImportWarnings) == 0 {
		t.Fatal("bundled profile changed semantics")
	}
	if p.Geoipurl != "https://github.com/runetfreedom/russia-blocked-geoip/releases/latest/download/geoip.dat" {
		t.Fatal("wrong GeoIP URL")
	}
}
