// Package transport multiplexes independent Telemost clients over one paced VP8
// publisher. The conference secret is shared: addressing is not user isolation.
package transport

import (
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"whitelist-bypass/relay/tunnel"
)

const (
	MaxClients       = 8 // Conservative ceiling below the current twelve-slot layout.
	maxPayload       = 256 * 1024
	headerSize       = 38
	hello       byte = 1
	welcome     byte = 2
	dataUp      byte = 3
	dataDown    byte = 4
	reject      byte = 5
	heartbeat        = 3 * time.Second
	idleTimeout      = 30 * time.Second
)

type ID [16]byte

var zero ID

// Writer is the one shared, paced VP8 publisher. TrySendData never blocks.
type Writer interface{ TrySendData([]byte) bool }

type packet struct {
	kind           byte
	client, server ID
	body           []byte
}

func encode(p packet) []byte {
	b := make([]byte, headerSize+len(p.body))
	copy(b, "TMMC")
	b[4], b[5] = 1, p.kind
	copy(b[6:22], p.client[:])
	copy(b[22:38], p.server[:])
	copy(b[38:], p.body)
	return b
}
func decode(b []byte) (packet, bool) {
	if len(b) < headerSize || len(b) > headerSize+maxPayload || string(b[:4]) != "TMMC" || b[4] != 1 {
		return packet{}, false
	}
	p := packet{kind: b[5], body: b[38:]}
	copy(p.client[:], b[6:22])
	copy(p.server[:], b[22:38])
	if p.client == zero {
		return packet{}, false
	}
	switch p.kind {
	case hello:
		return p, p.server == zero && len(p.body) == 0
	case welcome:
		return p, p.server != zero && len(p.body) == 0
	case reject:
		return p, p.server != zero && len(p.body) == 1 && (p.body[0] == 1 || p.body[0] == 2)
	case dataUp, dataDown:
		return p, p.server != zero && len(p.body) > 0
	}
	return packet{}, false
}

// Endpoint callbacks are serialized with incoming frames. They must not call
// HandleFrame or Close; install bridge callbacks before returning OnChannel.
type Endpoint struct {
	mu           sync.Mutex
	recvMu       sync.Mutex
	server       bool
	id           ID
	selected     ID
	writer       Writer
	obf          *tunnel.TunnelObfuscator
	channels     map[ID]*Channel
	order        []ID
	cursor       int
	stop         chan struct{}
	once         sync.Once
	done         chan struct{}
	onTimeout    func()
	onChannel    func(*Channel)
	logFn        func(string, ...any)
	started      time.Time
	warned       bool
	failed       bool
	retired      map[ID]bool
	retiredOrder []ID
}

func New(server bool, writer Writer, obf *tunnel.TunnelObfuscator, onChannel func(*Channel), logFn func(string, ...any)) (*Endpoint, error) {
	e := &Endpoint{server: server, writer: writer, obf: obf, channels: make(map[ID]*Channel), retired: make(map[ID]bool), stop: make(chan struct{}), done: make(chan struct{}), onChannel: onChannel, logFn: logFn, started: time.Now()}
	if _, err := rand.Read(e.id[:]); err != nil {
		return nil, err
	}
	if e.id == zero {
		return nil, errors.New("telemost: random session ID is zero")
	}
	go e.run()
	return e, nil
}

// Channel implements tunnel.DataTunnel. Each bridge owns a separate conn-ID
// namespace and bounded output queue. Configuration is per channel; aggregate
// publisher pacing is deliberately fixed, not last-client-wins.
type Channel struct {
	mu         sync.Mutex
	id         ID
	serverID   ID
	endpoint   *Endpoint
	queue      chan []byte
	incoming   chan []byte
	pending    []byte    // owned by endpoint scheduler under endpoint.mu
	lastSeen   time.Time // guarded by endpoint.mu
	welcomeDue bool
	closed     bool
	onData     func([]byte)
	onClose    func()
	stop       chan struct{}
	fps, batch int
}

func (c *Channel) ID() ID                    { return c.id }
func (c *Channel) SetOnData(fn func([]byte)) { c.mu.Lock(); c.onData = fn; c.mu.Unlock() }
func (c *Channel) SetOnClose(fn func())      { c.mu.Lock(); c.onClose = fn; c.mu.Unlock() }
func (c *Channel) Reconfigure(fps, batch int) {
	c.mu.Lock()
	c.fps = fps
	c.batch = batch
	c.mu.Unlock()
}
func (c *Channel) SendData(b []byte) {
	if len(b) == 0 || len(b) > maxPayload {
		return
	}
	copyB := append([]byte(nil), b...)
	select {
	case <-c.stop:
		return
	default:
	}
	select {
	case <-c.stop:
	case <-c.endpoint.stop:
	case c.queue <- copyB:
	}
}
func (c *Channel) deliver(b []byte) {
	select {
	case <-c.stop:
		return
	default:
	}
	select {
	case c.incoming <- append([]byte(nil), b...):
	default:
		// A blocked target socket is local to this client. Bound memory and close
		// only this channel instead of blocking every participant's track reader.
		e := c.endpoint
		e.mu.Lock()
		if e.channels[c.id] == c {
			e.retire(c.id)
			delete(e.channels, c.id)
		}
		if e.server {
			e.writer.TrySendData(encode(packet{kind: reject, client: c.id, server: e.id, body: []byte{2}}))
		}
		e.mu.Unlock()
		c.close()
	}
}
func (c *Channel) reader() {
	for {
		select {
		case <-c.stop:
			return
		case b := <-c.incoming:
			c.mu.Lock()
			fn, closed := c.onData, c.closed
			c.mu.Unlock()
			if !closed && fn != nil {
				fn(b)
			}
		}
	}
}

// Keep a bounded set of retired incarnations so delayed hello/data cannot
// resurrect a just-closed bridge. A reconnect creates a new random ID.
func (e *Endpoint) retire(id ID) {
	if e.retired[id] {
		return
	}
	if len(e.retiredOrder) >= 64 {
		delete(e.retired, e.retiredOrder[0])
		e.retiredOrder = e.retiredOrder[1:]
	}
	e.retired[id] = true
	e.retiredOrder = append(e.retiredOrder, id)
}

func (c *Channel) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.stop)
	fn := c.onClose
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}
func (e *Endpoint) newChannel(id, serverID ID, now time.Time) *Channel {
	c := &Channel{id: id, serverID: serverID, endpoint: e, queue: make(chan []byte, 32), incoming: make(chan []byte, 64), stop: make(chan struct{}), lastSeen: now, welcomeDue: e.server}
	e.channels[id] = c
	e.order = append(e.order, id)
	go c.reader()
	return c
}

// HandleFrame accepts reassembled VP8 frames from any remote track. Only an
// authenticated, versioned envelope can create or refresh a client session;
// unauthenticated keyframes/epochs never reset bridges.
func (e *Endpoint) HandleFrame(frame []byte) {
	e.recvMu.Lock()
	defer e.recvMu.Unlock()
	res := e.obf.DecodeUntracked(frame)
	if !res.HasFrame || res.SelfEcho || res.Keepalive {
		return
	}
	p, ok := decode(res.Payload)
	if !ok {
		return
	}
	e.mu.Lock()
	select {
	case <-e.stop:
		e.mu.Unlock()
		return
	default:
	}
	now := time.Now()
	var created *Channel
	var c *Channel
	if e.server {
		if p.kind == hello && !e.retired[p.client] {
			c = e.channels[p.client]
			if c == nil && len(e.channels) < MaxClients {
				c = e.newChannel(p.client, e.id, now)
				created = c
			}
			if c != nil {
				c.lastSeen = now
				c.welcomeDue = true
			} else {
				e.writer.TrySendData(encode(packet{kind: reject, client: p.client, server: e.id, body: []byte{1}}))
			}
		} else if p.kind == dataUp && p.server == e.id {
			c = e.channels[p.client]
			if c != nil {
				c.lastSeen = now
			}
		}
	} else if p.client == e.id && !e.failed {
		if p.kind == reject && (e.selected == zero || e.selected == p.server) {
			e.failed = true
			if e.logFn != nil {
				reason := "client limit reached"
				if p.body[0] == 2 {
					reason = "client receive queue overloaded"
				}
				e.logFn("telemost: creator rejected channel: %s", reason)
			}
		}
		if p.kind == welcome && !e.failed && (e.selected == zero || e.selected == p.server) {
			e.selected = p.server
			c = e.channels[e.id]
			if c == nil {
				c = e.newChannel(e.id, p.server, now)
				created = c
			}
			c.lastSeen = now
		} else if p.kind == dataDown && e.selected == p.server {
			c = e.channels[e.id]
			if c != nil {
				c.lastSeen = now
			}
		}
	}
	e.mu.Unlock()
	if created != nil && e.onChannel != nil {
		e.onChannel(created)
	}
	if c != nil && (p.kind == dataUp || p.kind == dataDown) {
		c.deliver(p.body)
	}
}

func (e *Endpoint) SetOnTimeout(fn func()) { e.mu.Lock(); e.onTimeout = fn; e.mu.Unlock() }

func (e *Endpoint) run() {
	defer close(e.done)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	lastHello := time.Time{}
	for {
		select {
		case <-e.stop:
			return
		case now := <-ticker.C:
			e.mu.Lock()
			if !e.server && now.Sub(lastHello) >= heartbeat {
				if e.writer.TrySendData(encode(packet{kind: hello, client: e.id})) {
					lastHello = now
				}
			}
			if !e.server && len(e.channels) == 0 && !e.warned && now.Sub(e.started) > idleTimeout {
				e.warned = true
				if e.logFn != nil {
					e.logFn("telemost: handshake timeout: creator unavailable, no video slot, or incompatible version; update both ends")
				}
			}
			timeoutCallback := e.onTimeout
			timedOut := !e.server && ((len(e.channels) == 0 && e.warned) || e.failed)
			var expired []*Channel
			var order []ID
			for _, id := range e.order {
				c := e.channels[id]
				if c == nil {
					continue
				}
				if now.Sub(c.lastSeen) > idleTimeout {
					delete(e.channels, id)
					e.retire(id)
					expired = append(expired, c)
					// A client endpoint cannot safely switch server incarnations with old
					// conn IDs. Reconnect signaling to create a fresh endpoint instead.
				} else {
					order = append(order, id)
				}
			}
			e.order = order
			// Rotate the first serviced client every tick. Each gets at most one
			// packet per pass, with retained pending data if the shared writer is full.
			n := len(e.order)
			if n > 0 {
				e.cursor = (e.cursor + 1) % n
				for i := 0; i < n; i++ {
					c := e.channels[e.order[(e.cursor+i)%n]]
					if c.welcomeDue {
						if !e.writer.TrySendData(encode(packet{kind: welcome, client: c.id, server: e.id})) {
							break
						}
						c.welcomeDue = false
					}
					if c.pending == nil {
						select {
						case c.pending = <-c.queue:
						default:
						}
					}
					if c.pending != nil {
						kind := dataUp
						if e.server {
							kind = dataDown
						}
						if !e.writer.TrySendData(encode(packet{kind: kind, client: c.id, server: c.serverID, body: c.pending})) {
							break
						}
						c.pending = nil
					}
				}
			}
			e.mu.Unlock()
			for _, c := range expired {
				c.close()
			}
			if !e.server && (timedOut || len(expired) > 0) {
				e.mu.Lock()
				e.failed = true
				e.mu.Unlock()
				if timeoutCallback != nil {
					timeoutCallback()
				}
				return
			}
		}
	}
}
func (e *Endpoint) Close() {
	e.once.Do(func() { close(e.stop) })
	<-e.done
	e.recvMu.Lock()
	defer e.recvMu.Unlock()
	e.mu.Lock()
	all := e.channels
	e.channels = make(map[ID]*Channel)
	e.order = nil
	e.mu.Unlock()
	for _, c := range all {
		c.close()
	}
}
