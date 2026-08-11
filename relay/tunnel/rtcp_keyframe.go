package tunnel

import (
	"sync"

	"github.com/pion/webrtc/v4"

	"whitelist-bypass/relay/common"
)

// Pion buffers inbound RTCP per RTP sender and discards it when nobody reads.
// The SFU asks for a fresh keyframe with PLI/FIR on that channel, so a track
// whose sender is never read looks broken to the SFU and loses its binding.
//
// The sender exists when the track is created, but the VP8 tunnel that owns
// the pacing is built later (on PeerConnection connected). Registering the
// sender against its track lets NewVP8DataTunnel pick it up on its own.

var (
	rtcpSourcesMu sync.Mutex
	rtcpSources   = map[webrtc.TrackLocal]*webrtc.RTPSender{}
)

// RegisterRTCPSource ties an RTP sender to the track it publishes, so the VP8
// tunnel built on that track later can drain the sender's RTCP.
func RegisterRTCPSource(track webrtc.TrackLocal, sender *webrtc.RTPSender) {
	if track == nil || sender == nil {
		return
	}
	rtcpSourcesMu.Lock()
	rtcpSources[track] = sender
	rtcpSourcesMu.Unlock()
}

func takeRTCPSource(track webrtc.TrackLocal) *webrtc.RTPSender {
	if track == nil {
		return nil
	}
	rtcpSourcesMu.Lock()
	defer rtcpSourcesMu.Unlock()
	sender := rtcpSources[track]
	delete(rtcpSources, track)
	return sender
}

// drainRTCP reads the sender's RTCP until it closes, asking the tunnel for a
// keyframe whenever the SFU requests one.
func (t *VP8DataTunnel) drainRTCP(sender *webrtc.RTPSender) {
	if sender == nil {
		return
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			select {
			case <-t.stopCh:
				return
			default:
			}
			n, _, err := sender.Read(buf)
			if err != nil {
				return
			}
			if !requestsKeyframe(buf[:n]) {
				continue
			}
			if common.Debug {
				t.logFn("vp8tunnel: RTCP keyframe request from SFU")
			}
			t.RequestKeyframe()
		}
	}()
}

// requestsKeyframe walks a compound RTCP packet looking for payload-specific
// feedback that asks for a fresh keyframe: PLI (PT=206, FMT=1) or FIR
// (PT=206, FMT=4).
func requestsKeyframe(buf []byte) bool {
	for len(buf) >= 4 {
		fmtField := buf[0] & 0x1f
		payloadType := buf[1]
		// The length field counts 32-bit words minus one, so the packet
		// occupies (length+1)*4 bytes including this header.
		pktLen := ((int(buf[2])<<8 | int(buf[3])) + 1) * 4
		if pktLen <= 0 || pktLen > len(buf) {
			return false
		}
		if payloadType == 206 && (fmtField == 1 || fmtField == 4) {
			return true
		}
		buf = buf[pktLen:]
	}
	return false
}
