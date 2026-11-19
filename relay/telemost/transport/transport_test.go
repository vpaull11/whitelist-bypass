package transport

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"whitelist-bypass/relay/tunnel"
)

func noLog(string, ...any) {}
func secret(t *testing.T) *tunnel.TunnelObfuscator {
	t.Helper()
	o, err := tunnel.NewTunnelObfuscator([]byte("local-test-conference"))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

type sent struct {
	obf *tunnel.TunnelObfuscator
	b   []byte
}
type bus struct {
	mu        sync.Mutex
	endpoints []*Endpoint
	packets   chan sent
	stop      chan struct{}
	done      chan struct{}
}
type busWriter struct {
	bus *bus
	obf *tunnel.TunnelObfuscator
}

func (w busWriter) TrySendData(b []byte) bool {
	select {
	case w.bus.packets <- sent{w.obf, append([]byte(nil), b...)}:
		return true
	default:
		return false
	}
}
func newBus(t *testing.T) *bus {
	t.Helper()
	b := &bus{packets: make(chan sent, 256), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		for {
			select {
			case <-b.stop:
				return
			case p := <-b.packets:
				frame := p.obf.EncodeData(p.b)
				b.mu.Lock()
				endpoints := append([]*Endpoint(nil), b.endpoints...)
				b.mu.Unlock()
				for _, e := range endpoints {
					e.HandleFrame(frame)
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(b.stop)
		<-b.done
		b.mu.Lock()
		all := append([]*Endpoint(nil), b.endpoints...)
		b.mu.Unlock()
		for _, e := range all {
			e.Close()
		}
	})
	return b
}
func (b *bus) add(t *testing.T, server bool, fn func(*Channel)) *Endpoint {
	t.Helper()
	o := secret(t)
	e, err := New(server, busWriter{b, o}, o, fn, noLog)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.endpoints = append(b.endpoints, e)
	b.mu.Unlock()
	return e
}
func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var v T
		return v
	}
}
func receive(t *testing.T, c *Channel) <-chan []byte {
	t.Helper()
	out := make(chan []byte, 256)
	c.SetOnData(func(b []byte) { out <- append([]byte(nil), b...) })
	return out
}
func TestEnvelopeValidation(t *testing.T) {
	id := ID{1}
	server := ID{2}
	for _, kind := range []byte{hello, welcome, dataUp, dataDown} {
		p := packet{kind: kind, client: id}
		if kind != hello {
			p.server = server
		}
		if kind == dataUp || kind == dataDown {
			p.body = []byte("data")
		}
		encoded := encode(p)
		got, ok := decode(encoded)
		if !ok || got.kind != p.kind || !bytes.Equal(got.body, p.body) {
			t.Fatal("round trip", kind)
		}
		for i := 0; i < headerSize; i++ {
			if _, ok := decode(encoded[:i]); ok {
				t.Fatal("truncated envelope accepted")
			}
		}
	}
	valid := encode(packet{kind: hello, client: id})
	for _, bad := range [][]byte{nil, []byte("legacy"), encode(packet{kind: 99, client: id}), encode(packet{kind: hello}), encode(packet{kind: welcome, client: id}), encode(packet{kind: dataUp, client: id, server: server}), encode(packet{kind: dataDown, client: id, server: server, body: make([]byte, maxPayload+1)})} {
		if _, ok := decode(bad); ok {
			t.Fatal("invalid envelope accepted")
		}
	}
	valid[4] = 1
	if _, ok := decode(valid); ok {
		t.Fatal("unknown version accepted")
	}
}
func TestClientsWithIdenticalConnectionIDs(t *testing.T) {
	b := newBus(t)
	b.add(t, true, func(c *Channel) {
		rb := tunnel.NewRelayBridge(c, "creator", 32768, noLog)
		c.SetOnClose(rb.Close)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	ready := make(chan *Channel, 2)
	b.add(t, false, func(c *Channel) { ready <- c })
	b.add(t, false, func(c *Channel) { ready <- c })
	a, c := wait(t, ready), wait(t, ready)
	ar, cr := receive(t, a), receive(t, c)
	both := []*Channel{a, c}
	outputs := []<-chan []byte{ar, cr}
	for i, ch := range both {
		ch.SendData(tunnel.EncodeVP8Config(12+i*12, 10+i*10, 1))
		ack := wait(t, outputs[i])
		tunnel.DecodeFrames(ack, func(id uint32, kind byte, payload []byte) {
			if id != 0 || kind != tunnel.MsgConfigAck {
				t.Errorf("wrong ACK %d/%d", id, kind)
			}
		})
		ch.SendData(tunnel.EncodeFrame(1, tunnel.MsgConnect, []byte(listener.Addr().String())))
		connected := wait(t, outputs[i])
		tunnel.DecodeFrames(connected, func(id uint32, kind byte, payload []byte) {
			if id != 1 || kind != tunnel.MsgConnectOK {
				t.Errorf("connect failed %d/%d %s", id, kind, payload)
			}
		})
	}
	for n := 0; n < 5; n++ {
		for i, ch := range both {
			ch.SendData(tunnel.EncodeFrame(1, tunnel.MsgData, bytes.Repeat([]byte{byte('a' + i)}, 256)))
		}
		for i, out := range outputs {
			frame := wait(t, out)
			var got []byte
			tunnel.DecodeFrames(frame, func(id uint32, kind byte, payload []byte) {
				if id != 1 || kind != tunnel.MsgData {
					t.Errorf("wrong message %d/%d", id, kind)
				}
				got = append(got, payload...)
			})
			if !bytes.Equal(got, bytes.Repeat([]byte{byte('a' + i)}, 256)) {
				t.Fatal("client traffic crossed", i)
			}
		}
	}
}
func TestRoutingRestartAndForeignFrames(t *testing.T) {
	b := newBus(t)
	serverChannels := make(chan *Channel, 8)
	server := b.add(t, true, func(c *Channel) { c.SetOnData(func(v []byte) { c.SendData(v) }); serverChannels <- c })
	clientChannels := make(chan *Channel, 8)
	client := b.add(t, false, func(c *Channel) { clientChannels <- c })
	a := wait(t, clientChannels)
	serverA := wait(t, serverChannels)
	out := receive(t, a)
	peer := secret(t)
	// Wrong direction, wrong destination and a forged epoch on an invalid tag
	// must never reach the channel or trigger a close/restart.
	for _, p := range []packet{
		{kind: dataUp, client: client.id, server: server.id, body: []byte("peer upload")},
		{kind: dataDown, client: ID{9}, server: server.id, body: []byte("other client")},
		{kind: dataDown, client: client.id, server: ID{9}, body: []byte("old server")},
	} {
		client.HandleFrame(peer.EncodeData(encode(p)))
	}
	corrupt := peer.EncodeData(encode(packet{kind: dataDown, client: client.id, server: server.id, body: []byte("bad tag")}))
	corrupt[len(corrupt)-1] ^= 1
	client.HandleFrame(corrupt)
	client.HandleFrame(peer.EncodeKeepalive(30))
	select {
	case <-out:
		t.Fatal("foreign frame delivered")
	default:
	}
	// Repeated hello keeps the same channel, not a new bridge.
	server.HandleFrame(client.obf.EncodeData(encode(packet{kind: hello, client: client.id})))
	select {
	case <-serverChannels:
		t.Fatal("duplicate bridge")
	default:
	}
	// Expiring one channel leaves another client's traffic alive.
	secondReady := make(chan *Channel, 1)
	second := b.add(t, false, func(c *Channel) { secondReady <- c })
	c := wait(t, secondReady)
	wait(t, serverChannels)
	cr := receive(t, c)
	closed := make(chan struct{})
	serverA.SetOnClose(func() { close(closed) })
	server.mu.Lock()
	serverA.lastSeen = time.Now().Add(-idleTimeout - time.Second)
	server.mu.Unlock()
	wait(t, closed)
	// Old data cannot register an expired client.
	server.HandleFrame(client.obf.EncodeData(encode(packet{kind: dataUp, client: client.id, server: server.id, body: []byte("stale")})))
	server.mu.Lock()
	_, exists := server.channels[client.id]
	server.mu.Unlock()
	if exists {
		t.Fatal("stale data recreated session")
	}
	c.SendData([]byte("surviving connection"))
	if string(wait(t, cr)) != "surviving connection" {
		t.Fatal("other client reset")
	}
	_ = second
	// Restarted joiner uses a new ID and a separate channel.
	client.Close()
	replacementReady := make(chan *Channel, 1)
	replacement := b.add(t, false, func(c *Channel) { replacementReady <- c })
	r := wait(t, replacementReady)
	if replacement.id == client.id {
		t.Fatal("reused incarnation")
	}
	rr := receive(t, r)
	r.SendData([]byte("restarted"))
	if string(wait(t, rr)) != "restarted" {
		t.Fatal("restart failed")
	}
}

type controlledWriter struct {
	mu      sync.Mutex
	accept  bool
	packets []packet
}

func (w *controlledWriter) TrySendData(b []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.accept {
		return false
	}
	p, _ := decode(b)
	w.packets = append(w.packets, p)
	return true
}
func TestCapacityBackpressureAndClose(t *testing.T) {
	w := &controlledWriter{}
	e, err := New(true, w, secret(t), nil, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	peer := secret(t)
	for i := 1; i <= MaxClients+1; i++ {
		e.HandleFrame(peer.EncodeData(encode(packet{kind: hello, client: ID{byte(i)}})))
	}
	e.mu.Lock()
	if len(e.channels) != MaxClients {
		t.Fatal("capacity not enforced")
	}
	c := e.channels[ID{1}]
	other := e.channels[ID{2}]
	e.mu.Unlock()
	for i := 0; i < applicationQueueDepth; i++ {
		c.SendData([]byte{byte(i)})
	}
	other.SendData([]byte("not starved"))
	w.mu.Lock()
	w.accept = true
	w.mu.Unlock()
	deadline := time.After(5 * time.Second)
	for {
		w.mu.Lock()
		found := false
		for _, p := range w.packets {
			if p.client == other.id && p.kind == dataDown {
				found = true
			}
		}
		w.mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("client starved")
		case <-time.After(time.Millisecond):
		}
	}
	w.mu.Lock()
	w.accept = false
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			c.SendData([]byte("blocked"))
		}
	}()
	e.Close()
	wait(t, done)
}

func TestHandshakeTimeout(t *testing.T) {
	e, err := New(false, &controlledWriter{}, secret(t), nil, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	timeout := make(chan struct{})
	e.SetOnTimeout(func() { close(timeout) })
	e.mu.Lock()
	e.started = time.Now().Add(-idleTimeout - time.Second)
	e.mu.Unlock()
	wait(t, timeout)
}

func TestUDPClientsWithIdenticalIDs(t *testing.T) {
	b := newBus(t)
	b.add(t, true, func(c *Channel) { rb := tunnel.NewRelayBridge(c, "creator", 32768, noLog); c.SetOnClose(rb.Close) })
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			udp.WriteTo(buf[:n], addr)
		}
	}()
	ready := make(chan *Channel, 2)
	b.add(t, false, func(c *Channel) { ready <- c })
	b.add(t, false, func(c *Channel) { ready <- c })
	a, c := wait(t, ready), wait(t, ready)
	ar, cr := receive(t, a), receive(t, c)
	addr := udp.LocalAddr().String()
	for i, ch := range []*Channel{a, c} {
		payload := append([]byte{byte(len(addr))}, []byte(addr)...)
		payload = append(payload, byte('a'+i))
		ch.SendData(tunnel.EncodeFrame(1, tunnel.MsgUDP, payload))
	}
	for i, out := range []<-chan []byte{ar, cr} {
		frame := wait(t, out)
		tunnel.DecodeFrames(frame, func(id uint32, kind byte, payload []byte) {
			if id != 1 || kind != tunnel.MsgUDPReply || !bytes.Equal(payload, []byte{byte('a' + i)}) {
				t.Errorf("UDP crossed: client=%d id=%d kind=%d body=%q", i, id, kind, payload)
			}
		})
	}
}

func TestSlowInboundClientDoesNotBlockOthers(t *testing.T) {
	w := &controlledWriter{accept: true}
	started, unblock := make(chan struct{}), make(chan struct{})
	e, err := New(true, w, secret(t), func(c *Channel) {
		if c.id == (ID{1}) {
			var once sync.Once
			c.SetOnData(func([]byte) { once.Do(func() { close(started) }); <-unblock })
			c.SetOnClose(func() { close(unblock) })
		} else {
			c.SetOnData(func(v []byte) { c.SendData(v) })
		}
	}, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	peer := secret(t)
	for i := 1; i <= 2; i++ {
		e.HandleFrame(peer.EncodeData(encode(packet{kind: hello, client: ID{byte(i)}})))
	}
	send := func(i byte) {
		e.mu.Lock()
		c := e.channels[ID{i}]
		e.mu.Unlock()
		if c != nil {
			c.deliver([]byte("data"))
		}
	}
	send(1)
	wait(t, started)
	for i := 0; i < incomingQueueDepth+1; i++ {
		send(1)
	}
	e.mu.Lock()
	_, exists := e.channels[ID{1}]
	e.mu.Unlock()
	if exists {
		t.Fatal("overloaded channel not removed")
	}
	send(2)
	deadline := time.After(5 * time.Second)
	for {
		w.mu.Lock()
		ok := false
		for _, p := range w.packets {
			if p.kind == dataDown && p.client == (ID{2}) {
				ok = true
			}
		}
		w.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("slow client blocked others")
		case <-time.After(time.Millisecond):
		}
	}
	// A delayed hello for the retired incarnation must not recreate its bridge.
	e.HandleFrame(peer.EncodeData(encode(packet{kind: hello, client: ID{1}})))
	e.mu.Lock()
	_, exists = e.channels[ID{1}]
	e.mu.Unlock()
	if exists {
		t.Fatal("retired hello recreated session")
	}
}

func TestRejectAndServerIncarnation(t *testing.T) {
	e, err := New(false, &controlledWriter{}, secret(t), nil, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	peer := secret(t)
	rejected := make(chan struct{})
	e.SetOnTimeout(func() { close(rejected) })
	e.HandleFrame(peer.EncodeData(encode(packet{kind: reject, client: e.id, server: ID{1}, body: []byte{1}})))
	wait(t, rejected)
	e.HandleFrame(peer.EncodeData(encode(packet{kind: welcome, client: e.id, server: ID{1}})))
	e.mu.Lock()
	n := len(e.channels)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("rejected endpoint reactivated")
	}
}
