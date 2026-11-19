package routing

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func testProfile() *Profile {
	return &Profile{Name: "RU-SPLIT", DomainStrategy: "IPIfNonMatch", RouteOrder: "block-proxy-direct", DnsHosts: map[string]string{"cloudflare-dns.com": "1.1.1.1", "dns.google": "8.8.8.8"}, DomesticDNSType: "DoH", DomesticDNSDomain: "https://dns.google/dns-query", DomesticDNSIP: "8.8.8.8", RemoteDNSType: "DoH", RemoteDNSDomain: "https://cloudflare-dns.com/dns-query", RemoteDNSIP: "1.1.1.1"}
}
func profileJSON(p *Profile) string { b, _ := json.Marshal(p); return string(b) }
func parsed(t *testing.T, p *Profile) *Profile {
	t.Helper()
	q, err := Parse(profileJSON(p))
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func TestHAPPParser(t *testing.T) {
	p := testProfile()
	p.ProxySites = []string{"geosite:youtube", "domain:cloudflare-dns.com"}
	p.DirectIP = []string{"geoip:private", "geoip:ru"}
	p.ProxyIP = []string{"geoip:ru-blocked", "1.1.1.1/32"}
	input := strings.ReplaceAll(profileJSON(p), `"FakeDNS":false`, `"FakeDNS":"false"`)
	input = strings.ReplaceAll(input, `"GlobalProxy":false`, `"GlobalProxy":"false"`)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		payload := enc.EncodeToString([]byte(input))
		payload = payload[:10] + " \n" + payload[10:]
		q, err := Parse("happ://routing/add/" + payload)
		if err != nil {
			t.Fatal(err)
		}
		if q.Name != "RU-SPLIT" || q.FakeDNS || q.GlobalProxy || !q.NeedGeoIP() || !q.NeedGeoSite() {
			t.Fatal("wrong normalization")
		}
	}
	bad := []string{"happ://routing/other/abc", "https://routing/add/abc", "happ://routing/add/!", strings.Repeat("x", MaxProfileSize*2+1), "null"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Fatalf("accepted invalid input %q", s[:min(len(s), 40)])
		}
	}
	for _, mutation := range []func(*Profile){func(p *Profile) { p.FakeDNS = true }, func(p *Profile) { p.RouteOrder = "proxy-direct" }, func(p *Profile) { p.DomainStrategy = "AsIs" }, func(p *Profile) { p.DomesticDNSType = "UDP" }, func(p *Profile) { p.RemoteDNSIP = "127.0.0.1" }, func(p *Profile) { p.Geoipurl = "http://example.com/geoip.dat" }} {
		p := testProfile()
		mutation(p)
		if _, err := Parse(profileJSON(p)); err == nil {
			t.Fatal("invalid field accepted")
		}
	}
	if _, err := Parse(strings.TrimSuffix(profileJSON(testProfile()), "}") + `,"Unsupported":"yes"}`); err == nil {
		t.Fatal("unknown field ignored")
	}
}
func TestRoutingPrecedence(t *testing.T) {
	p := parsed(t, testProfile())
	p.ProxySites = []string{"domain:youtube.com", "full:only.example", "regexp:^video[0-9]+\\.example$"}
	p.BlockSites = []string{"domain:blocked.example"}
	p.DirectIP = []string{"geoip:private", "5.0.0.0/8"}
	p.ProxyIP = []string{"5.1.2.0/24"}
	m, err := Compile(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain, ip string
		want       Action
	}{{"youtube.com", "5.2.0.1", Proxy}, {"WWW.YOUTUBE.COM.", "5.2.0.1", Proxy}, {"notyoutube.com", "5.2.0.1", Direct}, {"only.example", "9.9.9.9", Proxy}, {"sub.only.example", "9.9.9.9", Direct}, {"video123.example", "9.9.9.9", Proxy}, {"blocked.example", "5.1.2.1", Block}, {"unknown.example", "5.1.2.1", Proxy}, {"unknown.example", "192.168.1.2", Direct}, {"unknown.example", "9.9.9.9", Direct}} {
		if got := m.Decide(tc.domain, []netip.Addr{netip.MustParseAddr(tc.ip)}); got != tc.want {
			t.Errorf("%s %s got %s want %s", tc.domain, tc.ip, got, tc.want)
		}
	}
	p.GlobalProxy = true
	m, err = Compile(p, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Decide("unknown.example", nil) != Proxy {
		t.Fatal("global proxy fallback")
	}
	if domain, err := NormalizeDomain("ПРИМЕР.РФ."); err != nil || domain != "xn--e1afmkfd.xn--p1ai" {
		t.Fatalf("IDN: %s %v", domain, err)
	}
}
func wireBytes(tag int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(tag<<3|2))
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}
func wireInt(tag int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(tag<<3)), v)
}
func geoIPFixture(code string) []byte {
	cidr := append(wireBytes(1, []byte{5, 1, 2, 0}), wireInt(2, 24)...)
	group := append(wireBytes(1, []byte(code)), wireBytes(2, cidr)...)
	return wireBytes(1, group)
}
func geoSiteFixture(code string) []byte {
	attr := append(wireBytes(1, []byte("test")), wireInt(2, 1)...)
	domain := append(wireInt(1, 2), wireBytes(2, []byte("youtube.com"))...)
	domain = append(domain, wireBytes(3, attr)...)
	group := append(wireBytes(1, []byte(code)), wireBytes(2, domain)...)
	return wireBytes(1, group)
}
func TestGeoDatabases(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"geoip:ru-blocked"}
	p.ProxySites = []string{"geosite:youtube@test"}
	m, err := Compile(p, geoIPFixture("RU-BLOCKED"), geoSiteFixture("YOUTUBE"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Decide("www.youtube.com", nil) != Proxy || m.Decide("", []netip.Addr{netip.MustParseAddr("5.1.2.3")}) != Proxy {
		t.Fatal("geo rules not matched")
	}
	for _, db := range [][]byte{nil, {0xff}, {0x0a, 0xff, 0xff, 0xff, 0xff}, geoIPFixture("wrong")} {
		if _, err := Compile(p, db, geoSiteFixture("YOUTUBE")); err == nil {
			t.Fatal("bad/missing GeoIP silently accepted")
		}
	}
	p.ProxySites = []string{"geosite:youtube@missing"}
	if _, err := Compile(p, geoIPFixture("ru-blocked"), geoSiteFixture("youtube")); err == nil {
		t.Fatal("missing attribute accepted")
	}
	p.ProxySites = []string{"regexp:["}
	if _, err := Compile(p, geoIPFixture("ru-blocked"), nil); err == nil {
		t.Fatal("bad regexp accepted")
	}
}
func FuzzHAPPParser(f *testing.F) {
	f.Add(profileJSON(testProfile()))
	f.Add("happ://routing/add/e30=")
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > MaxProfileSize*2+1 {
			t.Skip()
		}
		p, err := Parse(input)
		if err == nil {
			_, _ = Compile(p, nil, nil)
		}
	})
}
func FuzzGeoWire(f *testing.F) {
	f.Add(geoIPFixture("ru"))
	f.Add(geoSiteFixture("youtube"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > MaxGeoSize {
			t.Skip()
		}
		_, _ = loadGeoIP(b, map[string]bool{"ru": true})
		_, _ = loadGeoSite(b, map[string]bool{"youtube": true})
	})
}

func TestProvidedGeositSpellingHasExplicitWarning(t *testing.T) {
	p := testProfile()
	p.ProxySites = []string{"geosit:ru-blocked"}
	normalized, err := Preview(profileJSON(p))
	if err != nil {
		t.Fatal(err)
	}
	var preview Profile
	if err = json.Unmarshal([]byte(normalized), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ProxySites[0] != "geosite:ru-blocked" || len(preview.ImportWarnings) == 0 {
		t.Fatal("correction or warning missing")
	}
	// A normalized profile stays valid and keeps the corrected selector.
	saved, err := Parse(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxySites[0] != "geosite:ru-blocked" {
		t.Fatal("correction was not persisted")
	}
}

func TestPrivateInverseAndRuleValidation(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"geoip:!private"}
	if p.NeedGeoIP() {
		t.Fatal("builtin inverse private unnecessarily requires download")
	}
	m := testMatcher(t, parsed(t, p))
	if m.Decide("", []netip.Addr{netip.MustParseAddr("9.9.9.9")}) != Proxy || m.Decide("", []netip.Addr{netip.MustParseAddr("192.168.1.2")}) != Direct {
		t.Fatal("inverse membership incorrect")
	}
	for _, rule := range []string{"unknown:value", "geosite:", "geosite:youtube@!ads", "regexp:["} {
		p := testProfile()
		p.ProxySites = []string{rule}
		if _, err := Preview(profileJSON(p)); err == nil {
			t.Fatalf("invalid rule %q passed preview", rule)
		}
	}
	p = testProfile()
	p.ProxySites = []string{"domain:proxy.example"}
	p.BlockIP = []string{"1.1.1.1"}
	m = testMatcher(t, p)
	if m.Decide("proxy.example", []netip.Addr{netip.MustParseAddr("1.1.1.1")}) != Block {
		t.Fatal("prior explicit IP block bypassed")
	}
	p.RouteOrder = "proxy-block-direct"
	m = testMatcher(t, p)
	if m.Decide("proxy.example", []netip.Addr{netip.MustParseAddr("1.1.1.1")}) != Proxy {
		t.Fatal("RouteOrder not respected")
	}
}

func TestIndexedRulesMatchLargeGroups(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"geoip:index"}
	p.ProxySites = []string{"geosite:index"}
	group := wireBytes(1, []byte("index"))
	siteGroup := wireBytes(1, []byte("index"))
	for i := 0; i < 64; i++ {
		cidr := append(wireBytes(1, []byte{5, byte(i), 0, 0}), wireInt(2, 16)...)
		group = append(group, wireBytes(2, cidr)...)
		domain := append(wireInt(1, 2), wireBytes(2, []byte(fmt.Sprintf("site%d.example", i)))...)
		siteGroup = append(siteGroup, wireBytes(2, domain)...)
	}
	m, err := Compile(p, wireBytes(1, group), wireBytes(1, siteGroup))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if m.Decide("", []netip.Addr{netip.AddrFrom4([4]byte{5, byte(i), 1, 2})}) != Proxy || m.Decide(fmt.Sprintf("www.site%d.example", i), nil) != Proxy {
			t.Fatal("indexed match failed", i)
		}
	}
	if m.Decide("www.notsite3.example", []netip.Addr{netip.MustParseAddr("6.1.2.3")}) != Direct {
		t.Fatal("index false positive")
	}
}

func TestDNSServiceNames(t *testing.T) {
	name, err := NormalizeDomain("_https._tcp.Example.COM.")
	if err != nil || name != "_https._tcp.example.com" {
		t.Fatal(name, err)
	}
	p := testProfile()
	p.ProxySites = []string{"domain:example.com"}
	m := testMatcher(t, p)
	if m.Decide(name, nil) != Proxy {
		t.Fatal("service name bypassed parent rule")
	}
}
