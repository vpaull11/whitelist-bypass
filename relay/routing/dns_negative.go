package routing

import (
	"encoding/binary"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type negativeEntry struct {
	answer  []byte
	expires time.Time
}

func (d *DNS) negativeAnswer(query []byte) []byte {
	if len(query) < 12 || len(query) > 4096 {
		return nil
	}
	key := string(query[2:])
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.negative[key]
	if !ok {
		return nil
	}
	if !now.Before(entry.expires) {
		delete(d.negative, key)
		return nil
	}
	b := append([]byte(nil), entry.answer...)
	binary.BigEndian.PutUint16(b[:2], binary.BigEndian.Uint16(query[:2]))
	// Do not extend the authoritative negative TTL on repeated cache hits.
	var msg dnsmessage.Message
	if msg.Unpack(b) != nil {
		delete(d.negative, key)
		return nil
	}
	remainingTime := time.Until(entry.expires)
	if remainingTime <= 0 {
		delete(d.negative, key)
		return nil
	}
	remaining := uint32(remainingTime / time.Second)
	for i := range msg.Authorities {
		if msg.Authorities[i].Header.TTL > remaining {
			msg.Authorities[i].Header.TTL = remaining
		}
		if soa, ok := msg.Authorities[i].Body.(*dnsmessage.SOAResource); ok && soa.MinTTL > remaining {
			soa.MinTTL = remaining
		}
	}
	b, _ = msg.Pack()
	return b
}
func (d *DNS) rememberNegative(query, answer []byte, m *dnsmessage.Message) {
	if len(query) < 12 || len(query) > 4096 || len(answer) > 4096 || len(m.Answers) != 0 || (m.RCode != dnsmessage.RCodeNameError && m.RCode != dnsmessage.RCodeSuccess) {
		return
	}
	ttl := uint32(300)
	found := false
	for _, resource := range m.Authorities {
		if soa, ok := resource.Body.(*dnsmessage.SOAResource); ok {
			found = true
			if soa.MinTTL < ttl {
				ttl = soa.MinTTL
			}
			if resource.Header.TTL < ttl {
				ttl = resource.Header.TTL
			}
		}
	}
	if !found || ttl == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.negative) >= 256 {
		var victim string
		var oldest time.Time
		for key, entry := range d.negative {
			if victim == "" || entry.expires.Before(oldest) {
				victim, oldest = key, entry.expires
			}
		}
		delete(d.negative, victim)
	}
	d.negative[string(query[2:])] = negativeEntry{answer: append([]byte(nil), answer...), expires: time.Now().Add(time.Duration(ttl) * time.Second)}
}
