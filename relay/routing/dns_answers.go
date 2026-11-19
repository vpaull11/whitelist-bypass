package routing

import (
	"errors"
	"net/netip"

	"golang.org/x/net/dns/dnsmessage"
)

// inspectAnswer only considers addresses belonging to the question's CNAME
// chain. Unrelated answer records must not poison the IP-to-domain cache.
func inspectAnswer(domain string, m *dnsmessage.Message) ([]netip.Addr, []string, uint32, error) {
	reachable := map[string]bool{domain: true}
	aliases := []string{}
	ttl := uint32(3600)
	edges := map[string]string{}
	for _, r := range m.Answers {
		if body, ok := r.Body.(*dnsmessage.CNAMEResource); ok {
			owner, e1 := NormalizeDomain(r.Header.Name.String())
			target, e2 := NormalizeDomain(body.CNAME.String())
			if e1 != nil || e2 != nil {
				return nil, nil, 0, errors.New("invalid CNAME")
			}
			if old, ok := edges[owner]; ok && old != target {
				return nil, nil, 0, errors.New("conflicting CNAME")
			}
			edges[owner] = target
		}
	}
	current := domain
	for depth := 0; ; depth++ {
		next, ok := edges[current]
		if !ok {
			break
		}
		if depth >= 16 || reachable[next] {
			return nil, nil, 0, errors.New("CNAME loop or chain too long")
		}
		reachable[next] = true
		aliases = append(aliases, next)
		current = next
	}
	var ips []netip.Addr
	for _, r := range m.Answers {
		owner, err := NormalizeDomain(r.Header.Name.String())
		if err != nil || !reachable[owner] {
			continue
		}
		if r.Header.TTL < ttl {
			ttl = r.Header.TTL
		}
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			ips = append(ips, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			ips = append(ips, netip.AddrFrom16(body.AAAA).Unmap())
		}
	}
	return ips, aliases, ttl, nil
}
func (d *DNS) chainAction(domain string, m *dnsmessage.Message) (Action, bool, error) {
	_, aliases, _, err := inspectAnswer(domain, m)
	if err != nil {
		return "", false, err
	}
	action, found := d.matcher.Domain(domain)
	for _, name := range aliases {
		if a, ok := d.matcher.Domain(name); ok {
			if found {
				action = d.matcher.Prefer(action, a)
			} else {
				action, found = a, true
			}
		}
	}
	return action, found, nil
}
