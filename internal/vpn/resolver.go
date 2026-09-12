package vpn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

const TTL = 30

type Forwarder func(ctx context.Context, name string) ([]netip.Addr, error)

type ZoneResolver struct {
	mu      sync.RWMutex
	zone    map[string][]netip.Addr
	forward Forwarder
}

var _ Resolver = (*ZoneResolver)(nil)

func NewResolver() *ZoneResolver { return &ZoneResolver{zone: map[string][]netip.Addr{}} }

func (r *ZoneResolver) SetZone(ctx context.Context, addrs []Address) error {
	zone := map[string][]netip.Addr{}
	sorted := append([]Address(nil), addrs...)
	sortAddresses(sorted)
	for _, a := range sorted {
		if !a.IP.IsValid() {
			return fmt.Errorf("vpn: zone: address for %q is not an address", a.Owner)
		}
		for _, n := range a.Names {
			name := Normalize(n)
			if name == "" {
				continue
			}
			if !IsInternal(name) {
				return fmt.Errorf("vpn: zone: %q is outside %s, which this resolver is not authoritative for", n, Suffix)
			}
			zone[name] = append(zone[name], a.IP)
		}
	}
	r.mu.Lock()
	r.zone = zone
	r.mu.Unlock()
	return nil
}

func (r *ZoneResolver) SetForwarder(f Forwarder) {
	r.mu.Lock()
	r.forward = f
	r.mu.Unlock()
}

func (r *ZoneResolver) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	n := Normalize(name)
	if !IsInternal(n) {
		return nil, fmt.Errorf("vpn: resolve %q: outside %s: %w", name, Suffix, ErrNXDOMAIN)
	}
	r.mu.RLock()
	addrs := r.zone[n]
	fwd := r.forward
	r.mu.RUnlock()
	if len(addrs) > 0 {
		return append([]netip.Addr(nil), addrs...), nil
	}
	if fwd == nil {
		return nil, fmt.Errorf("vpn: resolve %q: %w", name, ErrNXDOMAIN)
	}

	up, err := fwd(ctx, n)
	if err != nil {
		return nil, err
	}
	if len(up) == 0 {
		return nil, fmt.Errorf("vpn: resolve %q: %w", name, ErrNXDOMAIN)
	}
	return up, nil
}

func (r *ZoneResolver) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.zone))
	for n := range r.zone {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *ZoneResolver) Answer(query []byte) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, fmt.Errorf("vpn: parse dns query: %w", err)
	}
	q, err := p.Question()
	if err != nil {
		return nil, fmt.Errorf("vpn: parse dns question: %w", err)
	}

	var (
		rcode   = dnsmessage.RCodeNameError
		answers []netip.Addr
	)
	if q.Class == dnsmessage.ClassINET {
		if addrs, err := r.Resolve(context.Background(), q.Name.String()); err == nil {

			rcode = dnsmessage.RCodeSuccess
			if q.Type == dnsmessage.TypeA {
				for _, a := range addrs {
					if a.Is4() {
						answers = append(answers, a)
					}
				}
			}
		}
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: false,
		RCode:              rcode,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, fmt.Errorf("vpn: build dns response: %w", err)
	}
	if err := b.Question(q); err != nil {
		return nil, fmt.Errorf("vpn: build dns response: %w", err)
	}
	if err := b.StartAnswers(); err != nil {
		return nil, fmt.Errorf("vpn: build dns response: %w", err)
	}
	for _, a := range answers {
		var res dnsmessage.AResource
		copy(res.A[:], a.AsSlice())
		err := b.AResource(dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   TTL,
		}, res)
		if err != nil {
			return nil, fmt.Errorf("vpn: build dns response: %w", err)
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil, fmt.Errorf("vpn: build dns response: %w", err)
	}
	return out, nil
}

func serveDNS(pc net.PacketConn, r *ZoneResolver, logf func(string, ...any)) {
	buf := make([]byte, 1500)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		resp, err := r.Answer(buf[:n])
		if err != nil {

			logf("dns: query from %s: %v", src, err)
			continue
		}
		if _, err := pc.WriteTo(resp, src); err != nil {
			return
		}
	}
}
