package transport

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"whitelist-bypass/relay/tunnel"
)

// impairedBus is a deterministic network, not a lossless mock: it drops the
// first hello and welcome, every fifth datagram, delays packets out of order,
// and duplicates every seventh accepted packet in both directions.
type impairedBus struct {
	mu         sync.Mutex
	endpoints  []*Endpoint
	packets    chan sent
	stop, done chan struct{}
	blackhole  ID
}
type impairedWriter struct {
	bus  *impairedBus
	obf  *tunnel.TunnelObfuscator
	next time.Time // TrySendData is called only under this endpoint's mutex
}

func (w *impairedWriter) TrySendData(b []byte) bool {
	if time.Now().Before(w.next) {
		return false
	}
	select {
	case w.bus.packets <- sent{w.obf, append([]byte(nil), b...)}:
		w.next = time.Now().Add(2 * time.Millisecond)
		return true
	default:
		return false
	}
}
func newImpairedBus(t *testing.T, latency ...time.Duration) *impairedBus {
	t.Helper()
	b := &impairedBus{packets: make(chan sent, 256), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		type pending struct {
			p   sent
			due time.Time
		}
		var queue []pending
		counts := make(map[string]int)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-b.stop:
				return
			case p := <-b.packets:
				envelope, ok := decode(p.b)
				if !ok {
					continue
				}
				key := fmt.Sprintf("%x/%d", envelope.client, envelope.kind)
				counts[key]++
				n := counts[key]
				if n == 1 { // Lose the first handshake, DATA and ACK datagrams in each direction.
					continue
				}
				if n%5 == 0 {
					continue
				}
				delay := time.Duration((n*7)%17) * time.Millisecond
				if len(latency) > 0 {
					delay += latency[0]
				}
				due := time.Now().Add(delay)
				queue = append(queue, pending{p, due})
				if n%7 == 0 {
					queue = append(queue, pending{p, due.Add(3 * time.Millisecond)})
				}
			case now := <-ticker.C:
				remaining := queue[:0]
				for _, entry := range queue {
					if now.Before(entry.due) {
						remaining = append(remaining, entry)
						continue
					}
					b.mu.Lock()
					all := append([]*Endpoint(nil), b.endpoints...)
					blocked := b.blackhole
					b.mu.Unlock()
					p, _ := decode(entry.p.b)
					if blocked != zero && p.client == blocked {
						continue
					}
					frame := entry.p.obf.EncodeData(entry.p.b)
					for _, e := range all {
						e.HandleFrame(frame)
					}
				}
				queue = remaining
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
func (b *impairedBus) add(t *testing.T, server bool, fn func(*Channel)) *Endpoint {
	t.Helper()
	o := secret(t)
	e, err := New(server, &impairedWriter{bus: b, obf: o}, o, fn, noLog)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.endpoints = append(b.endpoints, e)
	b.mu.Unlock()
	return e
}
func eventually[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatal("impaired network timed out")
		var v T
		return v
	}
}
func TestReliableChannelsUnderLossReorderingAndDuplicates(t *testing.T) {
	b := newImpairedBus(t)
	b.add(t, true, func(c *Channel) { c.SetOnData(func(v []byte) { c.SendData(v) }) })
	ready := make(chan *Channel, 3)
	for i := 0; i < 3; i++ {
		b.add(t, false, func(c *Channel) { ready <- c })
	}
	channels := []*Channel{eventually(t, ready), eventually(t, ready), eventually(t, ready)}
	received := make([]<-chan []byte, 3)
	expected := make([][][]byte, 3)
	for i, c := range channels {
		received[i] = receive(t, c)
		expected[i] = [][]byte{
			tunnel.EncodeVP8Config(24, 30, 1),
			tunnel.EncodeFrame(1, tunnel.MsgConnect, []byte("fixture:443")),
			tunnel.EncodeFrame(1, tunnel.MsgConnectOK, nil),
			tunnel.EncodeFrame(1, tunnel.MsgData, bytes.Repeat([]byte{byte('a' + i)}, 32768)),
			bytes.Repeat([]byte{byte('A' + i)}, maxPayload),
			tunnel.EncodeFrame(1, tunnel.MsgClose, nil),
		}
		go func(c *Channel, messages [][]byte) {
			for _, v := range messages {
				c.SendData(v)
			}
		}(c, expected[i])
	}
	for i := range channels {
		for n, want := range expected[i] {
			if got := eventually(t, received[i]); !bytes.Equal(got, want) {
				t.Fatalf("client %d message %d corrupted/reordered (%d != %d)", i, n, len(got), len(want))
			}
		}
	}
	// Retransmissions and duplicates must never be exposed to applications.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-received[0]:
		t.Fatal("duplicate delivery")
	case <-received[1]:
		t.Fatal("duplicate delivery")
	case <-received[2]:
		t.Fatal("duplicate delivery")
	case <-timer.C:
	}
	// A blackholed client must not prevent another client's new messages.
	b.mu.Lock()
	b.blackhole = channels[0].id
	b.mu.Unlock()
	for i := 0; i < 32; i++ {
		channels[0].SendData([]byte("lost-client"))
	}
	channels[1].SendData([]byte("still alive"))
	if string(eventually(t, received[1])) != "still alive" {
		t.Fatal("client isolation lost")
	}
}

func TestRelayBridgeTCPThroughImpairedCarrier(t *testing.T) {
	b := newImpairedBus(t)
	b.add(t, true, func(c *Channel) { rb := tunnel.NewRelayBridge(c, "creator", 32768, noLog); c.SetOnClose(rb.Close) })
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err == nil {
			defer c.Close()
			io.Copy(c, c)
		}
	}()
	ready := make(chan *Channel, 1)
	b.add(t, false, func(c *Channel) { ready <- c })
	c := eventually(t, ready)
	out := receive(t, c)
	c.SendData(tunnel.EncodeFrame(1, tunnel.MsgConnect, []byte(echo.Addr().String())))
	opened := eventually(t, out)
	if len(opened) < 9 || opened[8] != tunnel.MsgConnectOK {
		t.Fatal("CONNECT lost", opened)
	}
	body := bytes.Repeat([]byte("ordered tcp payload\n"), 8192)
	go func() {
		for start := 0; start < len(body); start += 32768 {
			end := min(start+32768, len(body))
			c.SendData(tunnel.EncodeFrame(1, tunnel.MsgData, body[start:end]))
		}
	}()
	var got []byte
	for len(got) < len(body) {
		frame := eventually(t, out)
		tunnel.DecodeFrames(frame, func(id uint32, kind byte, payload []byte) {
			if id != 1 || kind != tunnel.MsgData {
				t.Errorf("unexpected frame %d/%d", id, kind)
			}
			got = append(got, payload...)
		})
	}
	if !bytes.Equal(got, body) {
		t.Fatal("TCP stream corrupted")
	}
	c.SendData(tunnel.EncodeFrame(1, tunnel.MsgClose, nil))
}

func TestKCPValidationAndStallBound(t *testing.T) {
	s := newReliableSession()
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b, reliableConv)
	b[4] = 82
	if !validSegment(b) {
		t.Fatal("ACK rejected")
	}
	for _, bad := range [][]byte{nil, {1, 2}, append(append([]byte(nil), b...), 1), bytes.Repeat([]byte{1}, segmentMTU+1)} {
		if validSegment(bad) {
			t.Fatal("malformed datagram accepted")
		}
	}
	b[4] = 81
	binary.LittleEndian.PutUint32(b[20:], 100)
	if validSegment(b) {
		t.Fatal("truncated datagram accepted")
	}
	q := make(chan []byte, applicationQueueDepth)
	q <- []byte("pending")
	if !s.pump(q, time.Now()) {
		t.Fatal("new session stalled")
	}
	s.progress = time.Now().Add(-idleTimeout - time.Second)
	if s.pump(q, time.Now()) {
		t.Fatal("heartbeat could hide stalled data forever")
	}
}

func TestFullWriterBackpressureBoundedAndCloseUnblocks(t *testing.T) {
	w := &controlledWriter{}
	e, err := New(true, w, secret(t), nil, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	peer := secret(t)
	e.HandleFrame(peer.EncodeData(encode(packet{kind: hello, client: ID{1}})))
	e.mu.Lock()
	c := e.channels[ID{1}]
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			c.SendData(bytes.Repeat([]byte{byte(i)}, maxPayload))
		}
	}()
	// Wait until a full application queue is held behind the bounded KCP window.
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		pending := c.reliable.kcp.WaitSnd()
		output := len(c.reliable.output)
		e.mu.Unlock()
		if pending >= maxPendingSegments && len(c.queue) == cap(c.queue) {
			maxFragments := (maxPayload + segmentMTU - 25) / (segmentMTU - 24)
			if pending > maxPendingSegments+maxFragments || output > segmentQueueDepth {
				t.Fatal("unbounded sender", pending, output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backpressure was not reached")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("producer bypassed bounded queues")
	default:
	}
	e.Close()
	eventually(t, done)
}

func TestMalformedDataAndOldVersionDoNotCreateChannel(t *testing.T) {
	e, err := New(true, &controlledWriter{}, secret(t), nil, noLog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	peer := secret(t)
	old := encode(packet{kind: hello, client: ID{1}})
	old[4] = 1
	e.HandleFrame(peer.EncodeData(old))
	e.HandleFrame(peer.EncodeData(encode(packet{kind: dataUp, client: ID{1}, server: e.id, body: []byte("not KCP")})))
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.channels) != 0 {
		t.Fatal("malformed or legacy data created a session")
	}
}

func TestRecoveredWindowFitsInboundBudget(t *testing.T) {
	// Do not start the reader yet: one repaired gap can release all these
	// messages before the application goroutine gets scheduled.
	c := &Channel{incoming: make(chan []byte, incomingQueueDepth), stop: make(chan struct{})}
	for i := 0; i < reliableWindow; i++ {
		c.deliver([]byte("recovered"))
	}
	if len(c.incoming) != reliableWindow {
		t.Fatal("recovered window was dropped")
	}
	if c.incomingBytes.Load() != int64(reliableWindow*len("recovered")) {
		t.Fatal("byte budget mismatch")
	}
}

func TestHighRTTConnectAndDataRetransmission(t *testing.T) {
	b := newImpairedBus(t, 125*time.Millisecond) // ~250ms RTT before jitter/loss
	b.add(t, true, func(c *Channel) { c.SetOnData(func(v []byte) { c.SendData(v) }) })
	ready := make(chan *Channel, 1)
	b.add(t, false, func(c *Channel) { ready <- c })
	c := eventually(t, ready)
	out := receive(t, c)
	for _, payload := range [][]byte{
		tunnel.EncodeFrame(1, tunnel.MsgConnectOK, nil),
		tunnel.EncodeFrame(1, tunnel.MsgData, bytes.Repeat([]byte("TLS bytes"), 4096)),
	} {
		c.SendData(payload)
		if got := eventually(t, out); !bytes.Equal(got, payload) {
			t.Fatal("high-RTT payload lost or corrupted")
		}
	}
}
