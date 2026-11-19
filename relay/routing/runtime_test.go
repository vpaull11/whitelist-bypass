package routing

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/net/dns/dnsmessage"
)

type protectFunc func(int) bool

func (f protectFunc) Protect(fd int) bool { return f(fd) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func answer(query []byte, ip [4]byte, ttl uint32) []byte {
	var q dnsmessage.Message
	q.Unpack(query)
	q.Response = true
	q.RecursionAvailable = true
	q.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl}, Body: &dnsmessage.AResource{A: ip}}}
	b, _ := q.Pack()
	return b
}
func query(name string) []byte {
	n, _ := dnsmessage.NewName(name + ".")
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	b, _ := q.Pack()
	return b
}
func mockDoH(calls *atomic.Int32, ip [4]byte) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		out := answer(b, ip, 60)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
	})}
}
func testMatcher(t *testing.T, p *Profile) *Matcher {
	t.Helper()
	m, err := Compile(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestDNSRoutesAndCache(t *testing.T) {
	p := testProfile()
	p.ProxySites = []string{"domain:proxy.example"}
	p.BlockSites = []string{"domain:block.example"}
	p.ProxyIP = []string{"5.1.2.0/24"}
	m := testMatcher(t, p)
	d := newDNS(p, m, protectFunc(func(int) bool { return false }), &fakeProxy{})
	var direct, remote atomic.Int32
	d.direct = mockDoH(&direct, [4]byte{9, 9, 9, 9})
	d.remote = mockDoH(&remote, [4]byte{1, 1, 1, 2})
	defer d.Close()
	q := query("proxy.example")
	if _, err := validateAnswer(q, d.Query(context.Background(), q)); err != nil {
		t.Fatal(err)
	}
	if remote.Load() != 1 || direct.Load() != 0 {
		t.Fatal("proxy domain leaked to domestic DNS")
	}
	if a, ok := d.Cached(netip.MustParseAddr("1.1.1.2")); !ok || a != Proxy {
		t.Fatal("proxy DNS cache missing")
	}
	q = query("direct.example")
	d.Query(context.Background(), q)
	if direct.Load() != 1 {
		t.Fatal("direct resolver not used")
	}
	q = query("block.example")
	var blocked dnsmessage.Message
	blocked.Unpack(d.Query(context.Background(), q))
	if blocked.RCode != dnsmessage.RCodeRefused || direct.Load() != 1 || remote.Load() != 1 {
		t.Fatal("blocked domain queried upstream")
	}
	d.direct = mockDoH(&direct, [4]byte{5, 1, 2, 3})
	d.Query(context.Background(), query("unknown.example"))
	if remote.Load() != 2 {
		t.Fatal("IPIfNonMatch failed to re-resolve proxy IP")
	}
	// Shared CDN addresses conservatively keep PROXY, never last-writer DIRECT.
	sameIP := netip.MustParseAddr("1.1.1.2")
	var reply dnsmessage.Message
	reply.Unpack(answer(query("direct.example"), [4]byte{1, 1, 1, 2}, 60))
	d.remember("direct.example", &reply)
	if a, _ := d.Cached(sameIP); a != Proxy {
		t.Fatal("shared-IP proxy decision overwritten")
	}
	d.mu.Lock()
	for domain, e := range d.cache[sameIP] {
		e.expires = time.Now().Add(-time.Second)
		d.cache[sameIP][domain] = e
	}
	d.mu.Unlock()
	if _, ok := d.Cached(sameIP); ok {
		t.Fatal("expired decision reused")
	}
	// DnsHosts does not require either DoH connection.
	before := direct.Load() + remote.Load()
	q = query("cloudflare-dns.com")
	var hosts dnsmessage.Message
	hosts.Unpack(d.Query(context.Background(), q))
	if len(hosts.Answers) != 1 || direct.Load()+remote.Load() != before {
		t.Fatal("DnsHosts not respected")
	}
}
func TestDoHErrorsDoNotFallBackDirect(t *testing.T) {
	p := testProfile()
	p.ProxySites = []string{"domain:proxy.example"}
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { return false }), &fakeProxy{})
	defer d.Close()
	var direct atomic.Int32
	d.direct = mockDoH(&direct, [4]byte{9, 9, 9, 9})
	d.remote = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("remote unavailable") })}
	var m dnsmessage.Message
	m.Unpack(d.Query(context.Background(), query("proxy.example")))
	if m.RCode != dnsmessage.RCodeServerFailure || direct.Load() != 0 {
		t.Fatal("unsafe DNS failover")
	}
}

type fakeProxy struct {
	tcp, udp atomic.Int32
	fail     bool
}

func (p *fakeProxy) DialContext(context.Context, *M.Metadata) (net.Conn, error) {
	p.tcp.Add(1)
	if p.fail {
		return nil, errors.New("proxy offline")
	}
	a, b := net.Pipe()
	go func() { defer b.Close(); io.Copy(b, b) }()
	return a, nil
}
func (p *fakeProxy) DialUDP(*M.Metadata) (net.PacketConn, error) {
	p.udp.Add(1)
	return nil, errors.New("mock UDP unavailable")
}
func TestRouterOutboundsAndProtection(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"1.1.1.1"}
	p.BlockIP = []string{"2.2.2.2"}
	out := &fakeProxy{fail: true}
	var protected atomic.Int32
	r, err := NewRouter(&Snapshot{Profile: p, Matcher: testMatcher(t, p)}, protectFunc(func(int) bool { protected.Add(1); return false }), out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		ip         string
		proxyCalls int32
	}{{"1.1.1.1", 1}, {"2.2.2.2", 1}, {"127.0.0.1", 1}} {
		if c, err := r.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr(tc.ip), DstPort: 8080}); err == nil {
			c.Close()
			t.Fatal("failed/blocked flow succeeded")
		}
		if out.tcp.Load() != tc.proxyCalls {
			t.Fatal("wrong outbound")
		}
	}
	if protected.Load() != 1 {
		t.Fatal("DIRECT protector was not invoked exactly once")
	}
	r.DialUDP(&M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr("1.1.1.1"), DstPort: 443})
	if out.udp.Load() != 1 {
		t.Fatal("UDP proxy route missing")
	}
	if c, err := r.DialUDP(&M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: 443}); err == nil {
		c.Close()
		t.Fatal("UDP protect failure ignored")
	}
	r.Close()
	if c, err := r.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("1.1.1.1"), DstPort: 80}); err == nil {
		c.Close()
		t.Fatal("closed router created connection")
	}
}
func TestDNSUDPAndTCPInterception(t *testing.T) {
	p := testProfile()
	out := &fakeProxy{}
	r, err := NewRouter(&Snapshot{Profile: p, Matcher: testMatcher(t, p)}, protectFunc(func(int) bool { return false }), out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var calls atomic.Int32
	r.dns.direct = mockDoH(&calls, [4]byte{9, 9, 9, 9})
	meta := &M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr(DNSAddress), DstPort: 53}
	pc, err := r.DialUDP(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pc.SetDeadline(time.Now().Add(time.Second))
	q := query("example.com")
	if _, err = pc.WriteTo(q, meta.UDPAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = validateAnswer(q, buf[:n]); err != nil {
		t.Fatal(err)
	}
	pc.SetReadDeadline(time.Now().Add(-time.Second))
	if _, _, err = pc.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("UDP deadline ignored", err)
	}
	meta.Network = M.TCP
	c, err := r.DialContext(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	wire := append([]byte{byte(len(q) >> 8), byte(len(q))}, q...)
	if _, err = c.Write(wire); err != nil {
		t.Fatal(err)
	}
	var size [2]byte
	if _, err = io.ReadFull(c, size[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(size[0])<<8|int(size[1]))
	if _, err = io.ReadFull(c, body); err != nil {
		t.Fatal(err)
	}
	if _, err = validateAnswer(q, body); err != nil {
		t.Fatal(err)
	}
	if out.tcp.Load() != 0 || out.udp.Load() != 0 {
		t.Fatal("DNS leaked to generic outbounds")
	}
}
func TestSnapshotRollbackAndOfflineLoad(t *testing.T) {
	root := t.TempDir()
	p := testProfile()
	p.ProxySites = []string{"domain:proxy.example"}
	if _, err := Prepare(root, profileJSON(p), nil, nil); err != nil {
		t.Fatal(err)
	}
	before, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	p.ProxySites = []string{"regexp:["}
	if _, err := Prepare(root, profileJSON(p), nil, nil); err == nil {
		t.Fatal("invalid profile published")
	}
	after, err := Load(root)
	if err != nil || before.ID != after.ID {
		t.Fatal("rollback failed", err)
	}
	if after.Matcher.Decide("proxy.example", nil) != Proxy {
		t.Fatal("offline snapshot invalid")
	}
	if err = Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(root); err == nil {
		t.Fatal("deleted profile still loaded")
	}
}
func TestDownloadLimitsAndUnsafeURLs(t *testing.T) {
	for _, raw := range []string{"http://example.com/data", "file:///etc/passwd", "https://localhost/data", "https://127.0.0.1/data", "https://user:secret@example.com/data"} {
		if _, err := ValidateDownloadURL(raw); err == nil {
			t.Fatal("unsafe URL accepted", raw)
		}
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: MaxGeoSize + 1, Body: io.NopCloser(strings.NewReader("data"))}, nil
	})}
	if err := download(context.Background(), client, "https://example.com/data", filepath.Join(t.TempDir(), "geoip.dat"), "geoip", nil); err == nil {
		t.Fatal("oversized download accepted")
	}
	// Default transport still rejects DNS-rebinding to literal private hosts.
	if _, err := downloadClient(nil).Get("https://127.0.0.1/data"); err == nil {
		t.Fatal("private dial accepted")
	}
}

func TestCNAMEAndUnrelatedAnswers(t *testing.T) {
	p := testProfile()
	p.ProxySites = []string{"domain:proxy.example"}
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { return false }), &fakeProxy{})
	defer d.Close()
	q := query("alias.example")
	var m dnsmessage.Message
	m.Unpack(answer(q, [4]byte{5, 5, 5, 5}, 60))
	alias, _ := dnsmessage.NewName("proxy.example.")
	m.Answers[0].Header.Name = alias
	cname := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 5}, Body: &dnsmessage.CNAMEResource{CNAME: alias}}
	m.Answers = append(m.Answers, cname)
	unrelated, _ := dnsmessage.NewName("unrelated.example.")
	m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: unrelated, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 3600}, Body: &dnsmessage.AResource{A: [4]byte{6, 6, 6, 6}}})
	d.remember("alias.example", &m)
	if a, ok := d.Cached(netip.MustParseAddr("5.5.5.5")); !ok || a != Proxy {
		t.Fatal("CNAME proxy decision missing")
	}
	if _, ok := d.Cached(netip.MustParseAddr("6.6.6.6")); ok {
		t.Fatal("unrelated answer poisoned cache")
	}
	d.mu.Lock()
	expires := d.cache[netip.MustParseAddr("5.5.5.5")]["alias.example"].expires
	d.mu.Unlock()
	if time.Until(expires) > 5*time.Second {
		t.Fatal("CNAME TTL ignored")
	}
	// A loop is rejected rather than interpreted as a direct answer.
	m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: alias, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 5}, Body: &dnsmessage.CNAMEResource{CNAME: m.Questions[0].Name}})
	if _, _, _, err := inspectAnswer("alias.example", &m); err == nil {
		t.Fatal("CNAME loop accepted")
	}
}

func TestProxyIPReresolutionKeepsProxyDecision(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"5.1.2.0/24"}
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { return false }), &fakeProxy{})
	defer d.Close()
	var direct, remote atomic.Int32
	d.direct = mockDoH(&direct, [4]byte{5, 1, 2, 3})
	d.remote = mockDoH(&remote, [4]byte{6, 6, 6, 6})
	d.Query(context.Background(), query("unknown.example"))
	if direct.Load() != 1 || remote.Load() != 1 {
		t.Fatal("expected both resolution stages")
	}
	if a, ok := d.Cached(netip.MustParseAddr("6.6.6.6")); !ok || a != Proxy {
		t.Fatal("proxy decision lost after remote IP changed")
	}
}

type cancelledProgress struct{}

func (cancelledProgress) OnProgress(string, int64) {}
func (cancelledProgress) IsCancelled() bool        { return true }
func TestCancelledUpdateAndSuccessfulReplacement(t *testing.T) {
	root := t.TempDir()
	p := testProfile()
	if _, err := Prepare(root, profileJSON(p), nil, nil); err != nil {
		t.Fatal(err)
	}
	old, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	p.Name = "Updated"
	if _, err := Prepare(root, profileJSON(p), cancelledProgress{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	saved, err := Load(root)
	if err != nil || saved.ID != old.ID {
		t.Fatal("cancel replaced snapshot")
	}
	if _, err := Prepare(root, profileJSON(p), nil, nil); err != nil {
		t.Fatal(err)
	}
	next, err := Load(root)
	if err != nil || next.Profile.Name != "Updated" || next.ID == old.ID {
		t.Fatal("replacement failed", err)
	}
	if _, err = os.Stat(filepath.Join(root, old.ID)); !os.IsNotExist(err) {
		t.Fatal("retired snapshot not removed", err)
	}
	// Already-loaded routers remain usable after the old files are removed.
	if old.Matcher.Decide("example.com", nil) != Direct {
		t.Fatal("loaded snapshot corrupted by replacement")
	}
}

func TestNegativeDNSCachePreservesIDAndExpiry(t *testing.T) {
	p := testProfile()
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { return false }), &fakeProxy{})
	defer d.Close()
	var calls atomic.Int32
	d.direct = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var m dnsmessage.Message
		m.Unpack(body)
		m.Response = true
		m.RCode = dnsmessage.RCodeNameError
		ns, _ := dnsmessage.NewName("ns.example.")
		mbox, _ := dnsmessage.NewName("hostmaster.example.")
		m.Authorities = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.SOAResource{NS: ns, MBox: mbox, MinTTL: 10}}}
		reply, _ := m.Pack()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(reply))}, nil
	})}
	q := query("missing.example")
	d.Query(context.Background(), q)
	q[1] = 43
	var cached dnsmessage.Message
	cached.Unpack(d.Query(context.Background(), q))
	if calls.Load() != 1 || cached.ID != 43 || cached.RCode != dnsmessage.RCodeNameError {
		t.Fatal("negative cache mismatch", calls.Load(), cached.ID)
	}
	d.mu.Lock()
	for key, entry := range d.negative {
		entry.expires = time.Now().Add(-time.Second)
		d.negative[key] = entry
	}
	d.mu.Unlock()
	d.Query(context.Background(), q)
	if calls.Load() != 2 {
		t.Fatal("negative TTL ignored")
	}
}

func TestProtectedDoHUsesTLSVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(answer(body, [4]byte{9, 9, 9, 9}, 60))
	}))
	defer server.Close()
	// Private bootstrap is a local-test-only configuration constructed without
	// Parse. Imported profiles reject private DoH destinations.
	p := testProfile()
	p.DomesticDNSDomain = server.URL + "/dns-query"
	p.DomesticDNSIP = "127.0.0.1"
	var protected atomic.Int32
	d := newDNS(p, testMatcher(t, p), protectFunc(func(int) bool { protected.Add(1); return true }), &fakeProxy{})
	defer d.Close()
	tr := d.direct.Transport.(*http.Transport)
	tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	q := query("direct.example")
	reply := d.Query(context.Background(), q)
	if _, err := validateAnswer(q, reply); err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	m.Unpack(reply)
	if m.RCode != dnsmessage.RCodeSuccess || protected.Load() == 0 {
		t.Fatal("protected DoH failed")
	}
	// No trust roots: verification must fail, not fall back to cleartext or proxy.
	d.direct.CloseIdleConnections()
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.RootCAs = x509.NewCertPool()
	q = query("other.example")
	m.Unpack(d.Query(context.Background(), q))
	if m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatal("TLS verification bypassed")
	}
}

func TestProxyTCPAndProtectedDirectUDP(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"1.1.1.1"}
	out := &fakeProxy{}
	var protected atomic.Int32
	r, err := NewRouter(&Snapshot{Profile: p, Matcher: testMatcher(t, p)}, protectFunc(func(int) bool { protected.Add(1); return true }), out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := r.DialContext(context.Background(), &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("1.1.1.1"), DstPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err = c.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	var response [4]byte
	if _, err = io.ReadFull(c, response[:]); err != nil || string(response[:]) != "echo" {
		t.Fatal("proxy stream failed", err)
	}
	if protected.Load() != 0 {
		t.Fatal("proxy payload opened direct socket")
	}
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		var b [32]byte
		n, addr, e := udp.ReadFrom(b[:])
		if e == nil {
			udp.WriteTo(b[:n], addr)
		}
	}()
	dest := udp.LocalAddr().(*net.UDPAddr)
	pc, err := r.DialUDP(&M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: uint16(dest.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pc.SetDeadline(time.Now().Add(time.Second))
	if _, err = pc.WriteTo([]byte("udp"), dest); err != nil {
		t.Fatal(err)
	}
	var b [32]byte
	n, _, err := pc.ReadFrom(b[:])
	if err != nil || string(b[:n]) != "udp" || protected.Load() != 1 {
		t.Fatal("protected direct UDP failed", err)
	}
	r.Close()
	before := out.tcp.Load()
	if _, err = r.DialContext(context.Background(), &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("1.1.1.1"), DstPort: 80}); err == nil || out.tcp.Load() != before {
		t.Fatal("closed router dialed again")
	}
	if _, err = c.Read(b[:]); err == nil {
		t.Fatal("Close left TCP connection alive")
	}
}
