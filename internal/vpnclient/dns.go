package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/plytz/caramelo/internal/vpn"
)

var (
	dnsTimeout  = 2 * time.Second
	dnsAttempts = 3
)

type dnsDialer func(ctx context.Context) (net.Conn, error)

func lookupA(ctx context.Context, dial dnsDialer, name string) ([]netip.Addr, error) {
	n := vpn.Normalize(name)
	if !vpn.IsInternal(n) && n != vpn.Domain {
		return nil, fmt.Errorf("%s is not a %s name: %w", name, vpn.Suffix, vpn.ErrNXDOMAIN)
	}
	q, err := dnsmessage.NewName(n + ".")
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid DNS name: %w", name, err)
	}
	var last error
	for attempt := 0; attempt < dnsAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		addrs, err := queryA(ctx, dial, q, uint16(attempt+1))
		switch {
		case err == nil:
			return addrs, nil
		case errors.Is(err, vpn.ErrNXDOMAIN):
			return nil, err
		}
		last = err
	}
	return nil, fmt.Errorf("resolve %s: %w", name, last)
}

func queryA(ctx context.Context, dial dnsDialer, name dnsmessage.Name, id uint16) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()

	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: false})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, fmt.Errorf("build a DNS query: %w", err)
	}
	if err := b.Question(question); err != nil {
		return nil, fmt.Errorf("build a DNS query: %w", err)
	}
	msg, err := b.Finish()
	if err != nil {
		return nil, fmt.Errorf("build a DNS query: %w", err)
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, fmt.Errorf("send a DNS query: %w", err)
	}

	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("no answer from the resolver: %w", err)
	}
	return parseA(buf[:n], id, name)
}

func parseA(msg []byte, id uint16, name dnsmessage.Name) ([]netip.Addr, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, fmt.Errorf("parse the DNS answer: %w", err)
	}
	if h.ID != id {
		return nil, fmt.Errorf("the resolver answered a different query (id %d, want %d)", h.ID, id)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, fmt.Errorf("parse the DNS answer: %w", err)
	}
	if h.RCode == dnsmessage.RCodeNameError {
		return nil, fmt.Errorf("%s: %w", trimDot(name.String()), vpn.ErrNXDOMAIN)
	}
	if h.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("the resolver refused %s (%s)", trimDot(name.String()), h.RCode)
	}
	var out []netip.Addr
	for {
		ah, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse the DNS answer: %w", err)
		}
		if ah.Type != dnsmessage.TypeA {
			if err := p.SkipAnswer(); err != nil {
				return nil, fmt.Errorf("parse the DNS answer: %w", err)
			}
			continue
		}
		r, err := p.AResource()
		if err != nil {
			return nil, fmt.Errorf("parse the DNS answer: %w", err)
		}
		out = append(out, netip.AddrFrom4(r.A))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", trimDot(name.String()), vpn.ErrNXDOMAIN)
	}
	return out, nil
}

func trimDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}
