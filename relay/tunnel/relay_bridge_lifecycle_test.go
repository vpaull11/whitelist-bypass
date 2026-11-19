package tunnel

import (
	"io"
	"net"
	"testing"
	"time"
)

type discardTunnel struct{}

func (discardTunnel) SendData([]byte)        {}
func (discardTunnel) SetOnData(func([]byte)) {}
func (discardTunnel) SetOnClose(func())      {}
func (discardTunnel) Reconfigure(int, int)   {}

// A Telemost channel can time out while its creator bridge is dialing. A
// successful late dial must not leave a socket/goroutine behind after Close.
func TestClosedBridgeDiscardsLateTCPDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerEOF := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			peerEOF <- err
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b [1]byte
		_, err = c.Read(b[:])
		peerEOF <- err
	}()
	rb := NewRelayBridge(discardTunnel{}, "creator", 4096, func(string, ...any) {})
	rb.Close()
	done := make(chan struct{})
	go func() { defer close(done); rb.connectTCP(1, listener.Addr().String()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("late dial not cleaned up")
	}
	select {
	case err := <-peerEOF:
		if err != io.EOF {
			t.Fatalf("peer did not see close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer still open")
	}
	tcp, _, _ := rb.Stats()
	if tcp != 0 {
		t.Fatalf("closed bridge retained %d sockets", tcp)
	}
}

func TestClosedBridgeDiscardsLateUDPSession(t *testing.T) {
	rb := NewRelayBridge(discardTunnel{}, "creator", 4096, func(string, ...any) {})
	rb.Close()
	addr := "127.0.0.1:9"
	payload := append([]byte{byte(len(addr))}, []byte(addr)...)
	payload = append(payload, 'x')
	rb.handleUDP(1, payload)
	_, udp, _ := rb.Stats()
	if udp != 0 {
		t.Fatalf("closed bridge retained %d UDP sessions", udp)
	}
}
