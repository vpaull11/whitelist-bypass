// Package routing implements the supported HAPP routing profile without a proxy core.
package routing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

const MaxProfileSize = 1 << 20
const MaxGeoSize = 256 << 20
const DNSAddress = "198.18.0.1"

type Action string

const (
	Block  Action = "block"
	Proxy  Action = "proxy"
	Direct Action = "direct"
)

type Profile struct {
	ImportWarnings                                                  []string `json:"ImportWarnings,omitempty"`
	Name                                                            string
	LastUpdated                                                     int64
	BlockIP, BlockSites, ProxyIP, ProxySites, DirectIP, DirectSites []string
	DnsHosts                                                        map[string]string
	DomainStrategy                                                  string
	DomesticDNSDomain, DomesticDNSIP, DomesticDNSType               string
	RemoteDNSDomain, RemoteDNSIP, RemoteDNSType                     string
	FakeDNS, GlobalProxy                                            bool
	Geoipurl, Geositeurl, RouteOrder                                string
}

func Parse(input string) (*Profile, error) {
	if len(input) > MaxProfileSize*2 {
		return nil, errors.New("routing profile too large")
	}
	input = strings.TrimSpace(input)
	var data []byte
	if strings.HasPrefix(input, "happ://routing/add/") {
		payload := strings.TrimPrefix(input, "happ://routing/add/")
		payload = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, payload)
		var err error
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			data, err = enc.DecodeString(payload)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, errors.New("invalid HAPP Base64 payload")
		}
	} else if strings.HasPrefix(input, "{") {
		data = []byte(input)
	} else {
		return nil, errors.New("expected happ://routing/add/<base64 JSON>")
	}
	if len(data) > MaxProfileSize || !utf8.Valid(data) {
		return nil, errors.New("invalid or oversized profile JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid profile JSON object")
	}
	for _, key := range []string{"FakeDNS", "GlobalProxy"} {
		if raw, ok := fields[key]; ok {
			if string(raw) == "null" {
				return nil, fmt.Errorf("%s must be boolean", key)
			}
			var text string
			if json.Unmarshal(raw, &text) == nil {
				if text != "true" && text != "false" {
					return nil, fmt.Errorf("invalid %s boolean", key)
				}
				fields[key] = json.RawMessage(text)
			}
		}
	}
	normalized, _ := json.Marshal(fields)
	d := json.NewDecoder(bytes.NewReader(normalized))
	d.DisallowUnknownFields()
	var p Profile
	if err := d.Decode(&p); err != nil {
		return nil, fmt.Errorf("unsupported or invalid routing field: %w", err)
	}
	p.ImportWarnings = nil
	for _, list := range []*[]string{&p.BlockSites, &p.ProxySites, &p.DirectSites} {
		corrected := false
		for i, rule := range *list {
			if strings.HasPrefix(rule, "geosit:") {
				(*list)[i] = "geosite:" + strings.TrimPrefix(rule, "geosit:")
				corrected = true
			}
		}
		if corrected {
			p.ImportWarnings = append(p.ImportWarnings, "Corrected geosit: spelling to geosite:. Review the group before saving.")
		}
	}
	if p.Name == "" || len(p.Name) > 128 {
		return nil, errors.New("profile Name must be 1..128 bytes")
	}
	if p.FakeDNS {
		return nil, errors.New("FakeDNS=true is not supported; use a profile with FakeDNS=false")
	}
	if p.DomainStrategy == "" {
		p.DomainStrategy = "IPIfNonMatch"
	}
	if p.DomainStrategy != "IPIfNonMatch" {
		return nil, errors.New("only DomainStrategy=IPIfNonMatch is supported")
	}
	if p.RouteOrder == "" {
		p.RouteOrder = "block-proxy-direct"
	}
	seen := map[string]bool{}
	for _, a := range strings.Split(p.RouteOrder, "-") {
		if a != "block" && a != "proxy" && a != "direct" || seen[a] {
			return nil, errors.New("RouteOrder must contain block, proxy, direct exactly once")
		}
		seen[a] = true
	}
	if len(seen) != 3 {
		return nil, errors.New("invalid RouteOrder")
	}
	count := 0
	for _, list := range [][]string{p.BlockIP, p.BlockSites, p.ProxyIP, p.ProxySites, p.DirectIP, p.DirectSites} {
		count += len(list)
		for _, v := range list {
			if v == "" || len(v) > 4096 {
				return nil, errors.New("invalid empty/oversized rule")
			}
		}
	}
	if count > 10000 || len(p.DnsHosts) > 256 {
		return nil, errors.New("too many routing rules")
	}
	hosts := make(map[string]string)
	for host, ip := range p.DnsHosts {
		h, err := NormalizeDomain(host)
		if err != nil {
			return nil, err
		}
		if _, err := netip.ParseAddr(ip); err != nil {
			return nil, errors.New("DnsHosts values must be IP addresses")
		}
		if previous, exists := hosts[h]; exists && previous != ip {
			return nil, errors.New("conflicting DnsHosts entries")
		}
		hosts[h] = ip
	}
	p.DnsHosts = hosts
	for _, dns := range [][3]string{{p.DomesticDNSType, p.DomesticDNSDomain, p.DomesticDNSIP}, {p.RemoteDNSType, p.RemoteDNSDomain, p.RemoteDNSIP}} {
		if dns[0] != "DoH" {
			return nil, errors.New("only DNSType=DoH is supported")
		}
		u, err := url.Parse(dns[1])
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("DNSDomain must be an HTTPS DoH URL without credentials")
		}
		declaredIP, declaredErr := netip.ParseAddr(dns[2])
		if declaredErr != nil || !publicIP(declaredIP) {
			return nil, errors.New("DNSIP must be a public bootstrap address")
		}
		bootstrap := dns[2]
		host, _ := NormalizeDomain(u.Hostname())
		if override, ok := p.DnsHosts[host]; ok {
			bootstrap = override
		}
		ip, err := netip.ParseAddr(bootstrap)
		if err != nil || !publicIP(ip) {
			return nil, errors.New("DNSIP must be a public bootstrap address")
		}
	}
	for _, u := range []string{p.Geoipurl, p.Geositeurl} {
		if u != "" {
			if _, err := ValidateDownloadURL(u); err != nil {
				return nil, err
			}
		}
	}
	if err := validateRuleSyntax(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
func validateRuleSyntax(p *Profile) error {
	validGroup := regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	for _, list := range [][]string{p.BlockIP, p.ProxyIP, p.DirectIP} {
		for _, rule := range list {
			if strings.HasPrefix(rule, "geoip:") {
				group := strings.TrimPrefix(strings.TrimPrefix(rule, "geoip:"), "!")
				if !validGroup.MatchString(group) {
					return fmt.Errorf("unsupported geoip selector %q", rule)
				}
			} else if prefix, err := netip.ParsePrefix(rule); err != nil {
				if _, err := netip.ParseAddr(rule); err != nil {
					return fmt.Errorf("invalid IP rule %q", rule)
				}
			} else if prefix.Addr().Is4In6() {
				return errors.New("IPv4-mapped CIDR is unsupported; use an IPv4 CIDR")
			}
		}
	}
	for _, list := range [][]string{p.BlockSites, p.ProxySites, p.DirectSites} {
		for _, rule := range list {
			if strings.HasPrefix(rule, "geosite:") {
				parts := strings.Split(strings.TrimPrefix(rule, "geosite:"), "@")
				if len(parts) > 2 || !validGroup.MatchString(parts[0]) || len(parts) == 2 && !validGroup.MatchString(parts[1]) {
					return fmt.Errorf("unsupported geosite selector %q", rule)
				}
			} else {
				kind := uint64(2)
				value := rule
				for prefix, k := range map[string]uint64{"domain:": 2, "full:": 3, "regexp:": 1, "keyword:": 0} {
					if strings.HasPrefix(rule, prefix) {
						kind, value = k, strings.TrimPrefix(rule, prefix)
						break
					}
				}
				if kind != 1 && strings.Contains(value, ":") {
					return fmt.Errorf("unsupported site rule %q", rule)
				}
				if _, err := newDomainRule(kind, value); err != nil {
					return fmt.Errorf("invalid site rule: %w", err)
				}
			}
		}
	}
	return nil
}
func NormalizeDomain(v string) (string, error) {
	v = strings.TrimSuffix(strings.ToLower(v), ".")
	if v == "" || len(v) > 253 || strings.ContainsAny(v, " /\\\x00:@") {
		return "", errors.New("invalid domain")
	}
	labels := strings.Split(v, ".")
	for i, label := range labels {
		// DNS service-discovery labels (_service._tcp) are not HTTP hostnames,
		// but must still resolve and inherit their parent domain's rules.
		prefix := ""
		if strings.HasPrefix(label, "_") {
			prefix = "_"
			label = strings.TrimPrefix(label, "_")
		}
		ascii, err := idna.Lookup.ToASCII(label)
		if err != nil || ascii == "" {
			return "", errors.New("invalid international domain")
		}
		labels[i] = prefix + ascii
		if len(labels[i]) > 63 {
			return "", errors.New("invalid domain label")
		}
	}
	v = strings.Join(labels, ".")
	if len(v) > 253 {
		return "", errors.New("domain exceeds DNS size limit")
	}
	return v, nil
}
func (p *Profile) NeedGeoIP() bool {
	for _, l := range [][]string{p.BlockIP, p.ProxyIP, p.DirectIP} {
		for _, s := range l {
			if strings.HasPrefix(s, "geoip:") && strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(s, "geoip:"), "!")) != "private" {
				return true
			}
		}
	}
	return false
}
func (p *Profile) NeedGeoSite() bool {
	for _, l := range [][]string{p.BlockSites, p.ProxySites, p.DirectSites} {
		for _, s := range l {
			if strings.HasPrefix(s, "geosite:") {
				return true
			}
		}
	}
	return false
}

type domainRule struct {
	kind  uint64
	value string
	re    *regexp.Regexp
}

func (r domainRule) matches(s string) bool {
	switch r.kind {
	case 0:
		return strings.Contains(s, r.value)
	case 1:
		return r.re.MatchString(s)
	case 2:
		return s == r.value || strings.HasSuffix(s, "."+r.value)
	case 3:
		return s == r.value
	}
	return false
}

type prefixGroup struct {
	bits      int
	v4        bool
	addresses map[netip.Addr]struct{}
}
type ipRule struct {
	prefixes []netip.Prefix
	groups   []prefixGroup
	inverse  bool
}

func (r *ipRule) index() {
	if len(r.prefixes) <= 16 {
		return
	}
	groups := map[int]*prefixGroup{}
	for _, p := range r.prefixes {
		key := p.Bits()
		if p.Addr().Is6() {
			key += 256
		}
		g := groups[key]
		if g == nil {
			g = &prefixGroup{bits: p.Bits(), v4: p.Addr().Is4(), addresses: make(map[netip.Addr]struct{})}
			groups[key] = g
		}
		g.addresses[p.Masked().Addr()] = struct{}{}
	}
	for _, g := range groups {
		r.groups = append(r.groups, *g)
	}
	r.prefixes = nil
}

func (r ipRule) matches(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, g := range r.groups {
		if g.v4 != ip.Is4() {
			continue
		}
		if _, ok := g.addresses[netip.PrefixFrom(ip, g.bits).Masked().Addr()]; ok {
			return !r.inverse
		}
	}
	for _, p := range r.prefixes {
		if p.Contains(ip.Unmap()) {
			return !r.inverse
		}
	}
	return r.inverse
}

type ruleSet struct {
	action       Action
	domains      []domainRule
	suffix, full map[string]struct{}
	ips          []ipRule
}
type Matcher struct {
	profile *Profile
	sets    []ruleSet
}

func newDomainRule(kind uint64, v string) (domainRule, error) {
	r := domainRule{kind: kind, value: v}
	var err error
	switch kind {
	case 0:
		r.value = strings.ToLower(v)
	case 1:
		r.re, err = regexp.Compile(v)
	case 2, 3:
		r.value, err = NormalizeDomain(v)
	default:
		err = errors.New("unsupported geosite domain type")
	}
	if len(v) > 4096 || v == "" {
		err = errors.New("invalid domain rule")
	}
	return r, err
}
func neededGeoGroups(p *Profile) (map[string]bool, map[string]bool) {
	needIPs, needSites := map[string]bool{}, map[string]bool{}
	for _, list := range [][]string{p.BlockIP, p.ProxyIP, p.DirectIP} {
		for _, s := range list {
			if strings.HasPrefix(s, "geoip:") {
				key := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(s, "geoip:"), "!"))
				if key != "private" {
					needIPs[strings.ToLower(key)] = true
				}
			}
		}
	}
	for _, list := range [][]string{p.BlockSites, p.ProxySites, p.DirectSites} {
		for _, s := range list {
			if strings.HasPrefix(s, "geosite:") {
				needSites[strings.ToLower(strings.SplitN(strings.TrimPrefix(s, "geosite:"), "@", 2)[0])] = true
			}
		}
	}
	return needIPs, needSites
}

func Compile(p *Profile, geoip, geosite []byte) (*Matcher, error) {
	ips, sites := map[string][]netip.Prefix{}, map[string][]geoDomain{}
	needIPs, needSites := neededGeoGroups(p)
	var err error
	if len(needIPs) > 0 {
		ips, err = loadGeoIP(geoip, needIPs)
		if err != nil {
			return nil, err
		}
	}
	if len(needSites) > 0 {
		sites, err = loadGeoSite(geosite, needSites)
		if err != nil {
			return nil, err
		}
	}
	return compileGroups(p, ips, sites)
}

func compileGroups(p *Profile, ips map[string][]netip.Prefix, sites map[string][]geoDomain) (*Matcher, error) {
	m := &Matcher{profile: p}
	for _, a := range strings.Split(p.RouteOrder, "-") {
		set := ruleSet{action: Action(a)}
		var il, dl []string
		switch set.action {
		case Block:
			il, dl = p.BlockIP, p.BlockSites
		case Proxy:
			il, dl = p.ProxyIP, p.ProxySites
		case Direct:
			il, dl = p.DirectIP, p.DirectSites
		}
		for _, s := range il {
			r := ipRule{}
			if strings.HasPrefix(s, "geoip:") {
				key := strings.TrimPrefix(s, "geoip:")
				r.inverse = strings.HasPrefix(key, "!")
				key = strings.ToLower(strings.TrimPrefix(key, "!"))
				if key == "private" {
					r.prefixes = privatePrefixes()
				} else {
					var found bool
					r.prefixes, found = ips[key]
					if !found {
						return nil, fmt.Errorf("missing geoip group %q", key)
					}
				}
			} else {
				prefix, err := netip.ParsePrefix(s)
				if err != nil {
					ip, e := netip.ParseAddr(s)
					if e != nil {
						return nil, fmt.Errorf("invalid IP rule %q", s)
					}
					ip = ip.Unmap()
					prefix = netip.PrefixFrom(ip, ip.BitLen())
				}
				r.prefixes = []netip.Prefix{prefix.Masked()}
			}
			r.index()
			set.ips = append(set.ips, r)
		}
		for _, s := range dl {
			if strings.HasPrefix(s, "geosite:") {
				selector := strings.SplitN(strings.ToLower(strings.TrimPrefix(s, "geosite:")), "@", 2)
				group, ok := sites[selector[0]]
				if !ok {
					return nil, fmt.Errorf("missing geosite group %q", selector[0])
				}
				added := 0
				for _, d := range group {
					if len(selector) == 2 && !d.attrs[selector[1]] {
						continue
					}
					r, err := newDomainRule(d.kind, d.value)
					if err != nil {
						return nil, err
					}
					set.domains = append(set.domains, r)
					added++
				}
				if added == 0 {
					return nil, fmt.Errorf("empty geosite selector %q", s)
				}
			} else {
				kind := uint64(2)
				v := s
				for prefix, k := range map[string]uint64{"domain:": 2, "full:": 3, "regexp:": 1, "keyword:": 0} {
					if strings.HasPrefix(s, prefix) {
						kind, v = k, strings.TrimPrefix(s, prefix)
						break
					}
				}
				if kind != 1 && strings.Contains(v, ":") {
					return nil, fmt.Errorf("unsupported site rule %q", s)
				}
				r, err := newDomainRule(kind, v)
				if err != nil {
					return nil, err
				}
				set.domains = append(set.domains, r)
			}
		}
		set.suffix = make(map[string]struct{})
		set.full = make(map[string]struct{})
		var patterns []domainRule
		for _, rule := range set.domains {
			switch rule.kind {
			case 2:
				set.suffix[rule.value] = struct{}{}
			case 3:
				set.full[rule.value] = struct{}{}
			default:
				patterns = append(patterns, rule)
			}
		}
		set.domains = patterns
		m.sets = append(m.sets, set)
	}
	return m, nil
}
func (m *Matcher) Domain(domain string) (Action, bool) {
	d, err := NormalizeDomain(domain)
	if err != nil {
		return "", false
	}
	for _, set := range m.sets {
		if _, ok := set.full[d]; ok {
			return set.action, true
		}
		for suffix := d; ; {
			if _, ok := set.suffix[suffix]; ok {
				return set.action, true
			}
			dot := strings.IndexByte(suffix, '.')
			if dot < 0 {
				break
			}
			suffix = suffix[dot+1:]
		}
		for _, r := range set.domains {
			if r.matches(d) {
				return set.action, true
			}
		}
	}
	return "", false
}
func (m *Matcher) IP(ips []netip.Addr) (Action, bool) {
	for _, set := range m.sets {
		for _, r := range set.ips {
			for _, ip := range ips {
				if r.matches(ip) {
					return set.action, true
				}
			}
		}
	}
	return "", false
}
func (m *Matcher) Fallback() Action {
	if m.profile.GlobalProxy {
		return Proxy
	}
	return Direct
}

// Prefer returns the higher-priority action configured by RouteOrder.
func (m *Matcher) Prefer(a, b Action) Action {
	for _, set := range m.sets {
		if set.action == a || set.action == b {
			return set.action
		}
	}
	return a
}
func (m *Matcher) Decide(domain string, ips []netip.Addr) Action {
	if a, ok := m.Domain(domain); ok {
		// A prior explicit IP block remains effective even when the domain is known.
		if ipAction, found := m.IP(ips); found && ipAction == Block {
			return m.Prefer(a, Block)
		}
		return a
	}
	if a, ok := m.IP(ips); ok {
		return a
	}
	return m.Fallback()
}
func privatePrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}
func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, p := range privatePrefixes() {
		if p.Contains(ip.Unmap()) {
			return false
		}
	}
	return true
}
func ValidateDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("geodata URL must use HTTPS without credentials/fragments")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return nil, errors.New("private geodata host forbidden")
	}
	if strings.EqualFold(u.Hostname(), "localhost") || !strings.Contains(u.Hostname(), ".") {
		return nil, errors.New("local geodata host forbidden")
	}
	return u, nil
}
