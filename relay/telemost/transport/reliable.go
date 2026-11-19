package transport

import (
	"encoding/binary"
	"time"

	kcp "github.com/xtaci/kcp-go/v5"
)

const (
	// Envelope (38), VP8/epoch (21), AEAD (40) and VP8 RTP descriptor
	// still fit below Pion's 1200-byte payload budget. Large relay messages
	// are fragmented by KCP, not by the unreliable video frame reassembler.
	segmentMTU            = 1080
	reliableWindow        = 256 // >= the 249 fragments of a maxPayload message
	maxPendingSegments    = 512
	segmentQueueDepth     = 256
	applicationQueueDepth = 8
	// A recovered gap can release a whole window of small messages at once.
	incomingQueueDepth = 2 * reliableWindow
	receiveByteLimit   = 2 * 1024 * 1024
	reliableInterval   = 10 * time.Millisecond
	reliableConv       = 0x544d0002
)

// reliableSession is owned exclusively by Endpoint.mu. KCP output is always
// nonblocking: a full carrier queue loses a datagram, which KCP retransmits.
// Application callbacks must run AFTER releasing Endpoint.mu.
type reliableSession struct {
	kcp      *kcp.KCP
	output   chan []byte
	updated  time.Time
	progress time.Time
}

func newReliableSession() *reliableSession {
	s := &reliableSession{output: make(chan []byte, segmentQueueDepth)}
	s.kcp = kcp.NewKCP(reliableConv, func(b []byte, n int) {
		if n <= 0 || n > segmentMTU {
			return
		}
		segment := append([]byte(nil), b[:n]...)
		select {
		case s.output <- segment:
		default:
		}
	})
	s.kcp.SetMtu(segmentMTU)
	s.kcp.WndSize(reliableWindow, reliableWindow)
	s.kcp.NoDelay(1, 10, 2, 1)
	return s
}

// Validate the complete datagram before Input: KCP can otherwise partially
// mutate state when a later segment in a malformed datagram is invalid.
func validSegment(b []byte) bool {
	if len(b) < 24 || len(b) > segmentMTU {
		return false
	}
	for len(b) > 0 {
		if len(b) < 24 || binary.LittleEndian.Uint32(b) != reliableConv {
			return false
		}
		n := int(binary.LittleEndian.Uint32(b[20:24]))
		cmd := b[4]
		if (cmd < 81 || cmd > 84) || n > len(b)-24 || (cmd != 81 && n != 0) {
			return false
		}
		b = b[24+n:]
	}
	return true
}

func (s *reliableSession) input(b []byte, now time.Time) ([][]byte, bool) {
	if !validSegment(b) {
		return nil, false
	}
	before := s.kcp.WaitSnd()
	if s.kcp.Input(b, kcp.IKCP_PACKET_REGULAR, true) != 0 {
		return nil, false
	}
	if s.kcp.WaitSnd() < before {
		s.progress = now
	}
	var messages [][]byte
	for {
		n := s.kcp.PeekSize()
		if n <= 0 {
			break
		}
		if n > maxPayload {
			return nil, false
		}
		message := make([]byte, n)
		if s.kcp.Recv(message) != n {
			return nil, false
		}
		messages = append(messages, message)
	}
	return messages, true
}

func (s *reliableSession) pump(queue <-chan []byte, now time.Time) bool {
	if s.kcp.WaitSnd() < maxPendingSegments {
		select {
		case b := <-queue:
			if s.kcp.WaitSnd() == 0 {
				s.progress = now
			}
			if s.kcp.Send(b) != 0 {
				return false
			}
		default:
		}
	}
	if now.Sub(s.updated) >= reliableInterval {
		s.kcp.Update()
		s.updated = now
	}
	return s.kcp.WaitSnd() == 0 || now.Sub(s.progress) <= idleTimeout
}
