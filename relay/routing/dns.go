package routing

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"golang.org/x/net/dns/dnsmessage"
)

type cacheEntry struct {
	action  Action
	expires time.Time
}
type DNS struct {
	profile        *Profile
	matcher        *Matcher
	direct, remote *http.Client
	mu             sync.Mutex
	cache          map[netip.Addr]map[string]cacheEntry
	slots          chan struct{}
	negative       map[string]negativeEntry
	googleTunnel   bool
}

func newDNS(p *Profile, m *Matcher, protect Protector, out proxy.Dialer, googleTunnel ...bool) *DNS {
	d := &DNS{profile: p, matcher: m, cache: make(map[netip.Addr]map[string]cacheEntry), negative: make(map[string]negativeEntry), slots: make(chan struct{}, 64)}
	client := func(remote bool, bootstrap, endpoint string) *http.Client {
		u, _ := url.Parse(endpoint)
		host, _ := NormalizeDomain(u.Hostname())
		if override, ok := p.DnsHosts[host]; ok && !d.googleTunnel {
			bootstrap = override
		}
		ip := netip.MustParseAddr(bootstrap)
		tr := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			_, portString, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			port, err := net.LookupPort("tcp", portString)
			if err != nil {
				return nil, err
			}
			if remote {
				conn, err := out.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: ip, DstPort: uint16(port)})
				if err != nil && d.googleTunnel && ctx.Err() == nil {
					return out.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("8.8.4.4"), DstPort: uint16(port)})
				}
				return conn, err
			}
			dialer := net.Dialer{Timeout: 10 * time.Second, Control: SocketControl(protect)}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), portString))
		}
		return &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("DoH redirects are not allowed") }}
	}
	if len(googleTunnel) > 0 && googleTunnel[0] {
		d.googleTunnel = true
		// Bootstrap must not be overridden by profile DnsHosts.
		d.remote = client(true, "8.8.8.8", "https://dns.google/dns-query")
		return d
	}
	d.direct = client(false, p.DomesticDNSIP, p.DomesticDNSDomain)
	d.remote = client(true, p.RemoteDNSIP, p.RemoteDNSDomain)
	return d
}
func (d *DNS) Close() {
	if d.direct != nil {
		d.direct.CloseIdleConnections()
	}
	if d.remote != nil {
		d.remote.CloseIdleConnections()
	}
}
func dnsError(query []byte, code dnsmessage.RCode) []byte {
	var m dnsmessage.Message
	if m.Unpack(query) != nil {
		return nil
	}
	m.Response = true
	m.RCode = code
	m.RecursionAvailable = true
	m.Answers = nil
	m.Authorities = nil
	m.Additionals = nil
	b, _ := m.Pack()
	return b
}
func (d *DNS) lookup(ctx context.Context, query []byte, remote bool) ([]byte, error) {
	endpoint, client := d.profile.DomesticDNSDomain, d.direct
	if d.googleTunnel {
		endpoint, client = "https://dns.google/dns-query", d.remote
	}
	if remote && !d.googleTunnel {
		endpoint, client = d.profile.RemoteDNSDomain, d.remote
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, errors.New("invalid DoH endpoint")
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("User-Agent", "RoutingClient/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("DoH request failed")
	}
	defer resp.Body.Close()
	contentType, _, contentErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != 200 || contentErr != nil || contentType != "application/dns-message" {
		return nil, errors.New("invalid DoH response")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil || len(b) > 65535 {
		return nil, errors.New("invalid DoH response size")
	}
	return b, nil
}
func validateAnswer(query, answer []byte) (*dnsmessage.Message, error) {
	var q, a dnsmessage.Message
	if q.Unpack(query) != nil || a.Unpack(answer) != nil || !a.Response || a.ID != q.ID || len(a.Questions) != 1 || len(q.Questions) != 1 || a.Questions[0] != q.Questions[0] {
		return nil, errors.New("mismatched DoH answer")
	}
	return &a, nil
}
func answerIPs(m *dnsmessage.Message) []netip.Addr {
	var ips []netip.Addr
	for _, r := range m.Answers {
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			ips = append(ips, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			ips = append(ips, netip.AddrFrom16(body.AAAA))
		}
	}
	return ips
}
func (d *DNS) Query(ctx context.Context, query []byte) []byte {
	if len(query) > 65535 {
		return nil
	}
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	default:
		return dnsError(query, dnsmessage.RCodeServerFailure)
	}
	var q dnsmessage.Message
	if q.Unpack(query) != nil || q.Response || len(q.Questions) != 1 || q.Questions[0].Class != dnsmessage.ClassINET {
		return dnsError(query, dnsmessage.RCodeFormatError)
	}
	domain, err := NormalizeDomain(q.Questions[0].Name.String())
	if q.Questions[0].Name.String() == "." {
		domain, err = "", nil
	}
	if err != nil {
		return dnsError(query, dnsmessage.RCodeFormatError)
	}
	action, matched := d.matcher.Domain(domain)
	if action == Block {
		return dnsError(query, dnsmessage.RCodeRefused)
	}
	if host, ok := d.profile.DnsHosts[domain]; ok && (q.Questions[0].Type == dnsmessage.TypeA || q.Questions[0].Type == dnsmessage.TypeAAAA) {
		ip := netip.MustParseAddr(host)
		q.Response = true
		q.RecursionAvailable = true
		q.Answers = nil
		q.Authorities = nil
		q.Additionals = nil
		hdr := dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 60}
		if q.Questions[0].Type == dnsmessage.TypeA && ip.Is4() {
			hdr.Type = dnsmessage.TypeA
			q.Answers = append(q.Answers, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AResource{A: ip.As4()}})
		}
		if q.Questions[0].Type == dnsmessage.TypeAAAA && ip.Is6() {
			hdr.Type = dnsmessage.TypeAAAA
			q.Answers = append(q.Answers, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: ip.As16()}})
		}
		b, _ := q.Pack()
		d.remember(domain, &q)
		return b
	}
	if cached := d.negativeAnswer(query); cached != nil {
		return cached
	}
	remote := action == Proxy || (!matched && d.profile.GlobalProxy)
	answer, err := d.lookup(ctx, query, remote)
	if err != nil {
		return dnsError(query, dnsmessage.RCodeServerFailure)
	}
	parsed, err := validateAnswer(query, answer)
	if err != nil {
		return dnsError(query, dnsmessage.RCodeServerFailure)
	}
	effective, domainMatched, err := d.chainAction(domain, parsed)
	if err != nil {
		return dnsError(query, dnsmessage.RCodeServerFailure)
	}
	if domainMatched && effective == Block {
		return dnsError(query, dnsmessage.RCodeRefused)
	}
	ips, _, _, err := inspectAnswer(domain, parsed)
	if err != nil {
		return dnsError(query, dnsmessage.RCodeServerFailure)
	}
	forceProxy := domainMatched && effective == Proxy
	if !domainMatched {
		if a, found := d.matcher.IP(ips); found {
			if a == Block {
				return dnsError(query, dnsmessage.RCodeRefused)
			}
			forceProxy = a == Proxy
		}
	}
	if forceProxy && !remote && !d.googleTunnel {
		answer, err = d.lookup(ctx, query, true)
		if err != nil {
			return dnsError(query, dnsmessage.RCodeServerFailure)
		}
		parsed, err = validateAnswer(query, answer)
		if err != nil {
			return dnsError(query, dnsmessage.RCodeServerFailure)
		}
		a, found, e := d.chainAction(domain, parsed)
		if e != nil {
			return dnsError(query, dnsmessage.RCodeServerFailure)
		}
		if found && a == Block && d.matcher.Prefer(Block, Proxy) == Block {
			return dnsError(query, dnsmessage.RCodeRefused)
		}
	}
	d.rememberAs(domain, parsed, forceProxy)
	d.rememberNegative(query, answer, parsed)
	return answer
}
func (d *DNS) remember(domain string, m *dnsmessage.Message) { d.rememberAs(domain, m, false) }
func (d *DNS) rememberAs(domain string, m *dnsmessage.Message, forceProxy bool) {
	if m.RCode != dnsmessage.RCodeSuccess {
		return
	}
	now := time.Now()
	ips, aliases, minTTL, err := inspectAnswer(domain, m)
	if err != nil || minTTL == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ip := range ips {
		entries := d.cache[ip]
		if entries == nil {
			if len(d.cache) >= 8192 {
				var victim netip.Addr
				var oldest time.Time
				for addr, records := range d.cache {
					for _, e := range records {
						if !victim.IsValid() || e.expires.Before(oldest) {
							victim, oldest = addr, e.expires
						}
					}
				}
				delete(d.cache, victim)
			}
			entries = make(map[string]cacheEntry)
			d.cache[ip] = entries
		}
		for name, entry := range entries {
			if !now.Before(entry.expires) {
				delete(entries, name)
			}
		}
		// Never silently replace a conflicting domain with DIRECT. A bounded
		// overflow is conservatively represented as PROXY until its TTL expires.
		a := d.matcher.Decide(domain, []netip.Addr{ip})
		for _, alias := range aliases {
			if aliasAction, ok := d.matcher.Domain(alias); ok {
				a = d.matcher.Prefer(a, aliasAction)
			}
		}
		if forceProxy && a != Block {
			a = Proxy
		}
		key := domain
		if len(entries) >= 16 {
			key = "*"
			a = Proxy
		}
		entries[key] = cacheEntry{action: a, expires: now.Add(time.Duration(minTTL) * time.Second)}
	}
}
func (d *DNS) Cached(ip netip.Addr) (Action, bool) {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := d.cache[ip.Unmap()]
	found := false
	action := Direct
	for domain, e := range entries {
		if !now.Before(e.expires) {
			delete(entries, domain)
			continue
		}
		found = true
		if e.action == Block {
			return Block, true
		}
		if e.action == Proxy {
			action = Proxy
		}
	}
	if len(entries) == 0 {
		delete(d.cache, ip.Unmap())
	}
	return action, found
}
func (d *DNS) serveTCP(ctx context.Context, c net.Conn) {
	defer c.Close()
	var head [2]byte
	for {
		c.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		size := int(binary.BigEndian.Uint16(head[:]))
		if size < 12 {
			return
		}
		query := make([]byte, size)
		if _, err := io.ReadFull(c, query); err != nil {
			return
		}
		answer := d.Query(ctx, query)
		if len(answer) == 0 {
			return
		}
		binary.BigEndian.PutUint16(head[:], uint16(len(answer)))
		if _, err := c.Write(append(head[:], answer...)); err != nil {
			return
		}
	}
}
