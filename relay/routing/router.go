package routing

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"golang.org/x/net/dns/dnsmessage"
)

type Router struct {
	snapshot    *Snapshot
	protector   Protector
	proxy       proxy.Dialer
	dns         *DNS
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	connections map[*owned]bool
}
type owned struct{ close func() error }

func NewRouter(s *Snapshot, p Protector, out proxy.Dialer, googleTunnel ...bool) (*Router, error) {
	if s == nil || s.Matcher == nil || p == nil || out == nil {
		return nil, errors.New("routing requires a valid profile, protector and proxy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{snapshot: s, protector: p, proxy: out, ctx: ctx, cancel: cancel, connections: make(map[*owned]bool)}
	r.dns = newDNS(s.Profile, s.Matcher, p, out, googleTunnel...)
	return r, nil
}
func (r *Router) own(close func() error) (*owned, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.connections) >= 512 {
		close()
		return nil, errors.New("router closed or flow limit reached")
	}
	o := &owned{close: close}
	r.connections[o] = true
	return o, nil
}
func (r *Router) forget(o *owned) { r.mu.Lock(); delete(r.connections, o); r.mu.Unlock() }

type trackedConn struct {
	net.Conn
	router *Router
	owned  *owned
	once   sync.Once
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); c.router.forget(c.owned) })
	return err
}

type trackedPacket struct {
	net.PacketConn
	router *Router
	owned  *owned
	once   sync.Once
}

func (c *trackedPacket) Close() error {
	var err error
	c.once.Do(func() { err = c.PacketConn.Close(); c.router.forget(c.owned) })
	return err
}
func (r *Router) track(c net.Conn) (net.Conn, error) {
	o, err := r.own(c.Close)
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: c, router: r, owned: o}, nil
}
func (r *Router) trackPacket(c net.PacketConn) (net.PacketConn, error) {
	o, err := r.own(c.Close)
	if err != nil {
		return nil, err
	}
	return &trackedPacket{PacketConn: c, router: r, owned: o}, nil
}
func (r *Router) action(ip netip.Addr) Action {
	ipAction, ipMatched := r.snapshot.Matcher.IP([]netip.Addr{ip})
	if a, ok := r.dns.Cached(ip); ok {
		if ipMatched && ipAction == Block {
			return r.snapshot.Matcher.Prefer(a, Block)
		}
		return a
	}
	return r.snapshot.Matcher.Decide("", []netip.Addr{ip})
}
func (r *Router) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	if r.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	if m.DstPort == 53 {
		a, b := net.Pipe()
		c, err := r.track(a)
		if err != nil {
			b.Close()
			return nil, err
		}
		go r.dns.serveTCP(r.ctx, b)
		return c, nil
	}
	var c net.Conn
	var err error
	switch r.action(m.DstIP) {
	case Block:
		return nil, errors.New("destination blocked by routing profile")
	case Proxy:
		c, err = r.proxy.DialContext(ctx, m)
	default:
		d := net.Dialer{Timeout: 10 * time.Second, Control: SocketControl(r.protector)}
		c, err = d.DialContext(ctx, "tcp", m.DestinationAddress())
	}
	if err != nil {
		return nil, err
	}
	return r.track(c)
}
func (r *Router) DialUDP(m *M.Metadata) (net.PacketConn, error) {
	if r.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if m.DstPort == 53 {
		return r.trackPacket(newDNSPacket(r.ctx, r.dns, m.UDPAddr()))
	}
	var c net.PacketConn
	var err error
	switch r.action(m.DstIP) {
	case Block:
		return nil, errors.New("destination blocked by routing profile")
	case Proxy:
		c, err = r.proxy.DialUDP(m)
	default:
		lc := net.ListenConfig{Control: SocketControl(r.protector)}
		network := "udp4"
		if m.DstIP.Is6() {
			network = "udp6"
		}
		c, err = lc.ListenPacket(r.ctx, network, ":0")
	}
	if err != nil {
		return nil, err
	}
	return r.trackPacket(c)
}
func (r *Router) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	all := r.connections
	r.connections = make(map[*owned]bool)
	r.mu.Unlock()
	r.cancel()
	r.dns.Close()
	for c := range all {
		c.close()
	}
}

type dnsPacket struct {
	ctx                         context.Context
	cancel                      context.CancelFunc
	dns                         *DNS
	remote                      net.Addr
	answers                     chan []byte
	slots                       chan struct{}
	mu                          sync.Mutex
	readDeadline, writeDeadline time.Time
	changed                     chan struct{}
}

func newDNSPacket(ctx context.Context, d *DNS, remote net.Addr) *dnsPacket {
	ctx, cancel := context.WithCancel(ctx)
	return &dnsPacket{ctx: ctx, cancel: cancel, dns: d, remote: remote, answers: make(chan []byte, 8), slots: make(chan struct{}, 8), changed: make(chan struct{})}
}
func (c *dnsPacket) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		deadline, changed := c.readDeadline, c.changed
		c.mu.Unlock()
		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-c.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case answer := <-c.answers:
			if timer != nil {
				timer.Stop()
			}
			n := copy(b, answer)
			return n, c.remote, nil
		}
	}
}
func (c *dnsPacket) WriteTo(b []byte, addr net.Addr) (int, error) {
	if len(b) > 4096 {
		return 0, errors.New("oversized DNS packet")
	}
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	select {
	case <-c.ctx.Done():
		return 0, net.ErrClosed
	case c.slots <- struct{}{}:
	default:
		return 0, errors.New("DNS query limit reached")
	}
	query := append([]byte(nil), b...)
	go func() {
		defer func() { <-c.slots }()
		ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
		defer cancel()
		answer := c.dns.Query(ctx, query)
		if len(answer) > 4096 {
			var msg dnsmessage.Message
			if msg.Unpack(answer) == nil {
				msg.Truncated = true
				msg.Answers = nil
				msg.Authorities = nil
				msg.Additionals = nil
				answer, _ = msg.Pack()
			}
		}
		if len(answer) == 0 {
			return
		}
		select {
		case c.answers <- answer:
		case <-c.ctx.Done():
		}
	}()
	return len(b), nil
}
func (c *dnsPacket) Close() error        { c.cancel(); return nil }
func (c *dnsPacket) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.ParseIP(DNSAddress), Port: 53} }
func (c *dnsPacket) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	return nil
}
func (c *dnsPacket) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	return nil
}
func (c *dnsPacket) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}
