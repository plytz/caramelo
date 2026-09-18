package fleet

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

const TicketPrefix = "caramelo-join-v1."

type Ticket struct {
	Hub string `json:"hub"`

	Fleet string `json:"fleet"`

	Endpoint string `json:"endpoint"`

	PublicKey string `json:"public_key"`

	Address string `json:"address"`

	Peer string `json:"peer"`

	Range string `json:"range"`

	Secret string `json:"secret"`
}

func (t Ticket) Encode() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("fleet: encode a join ticket: %w", err)
	}
	return TicketPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func ParseTicket(s string) (Ticket, error) {
	s = strings.TrimSpace(s)
	body, ok := strings.CutPrefix(s, TicketPrefix)
	if !ok {
		return Ticket{}, fmt.Errorf("fleet: that is not a join token: one starts with %q and comes from `caramelo member token` on the hub", TicketPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Ticket{}, fmt.Errorf("fleet: that join token is damaged — copy the whole line: %w", err)
	}
	var t Ticket
	if err := json.Unmarshal(b, &t); err != nil {
		return Ticket{}, fmt.Errorf("fleet: that join token is damaged — copy the whole line: %w", err)
	}
	if err := t.Validate(); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func (t Ticket) Validate() error {
	if strings.TrimSpace(t.Hub) == "" {
		return fmt.Errorf("fleet: a join token must name the hub")
	}
	if strings.TrimSpace(t.Fleet) == "" {
		return fmt.Errorf("fleet: a join token must name the fleet the hub is offering")
	}
	if strings.TrimSpace(t.Secret) == "" {
		return fmt.Errorf("fleet: a join token with no secret is not a token")
	}
	if strings.TrimSpace(t.PublicKey) == "" {
		return fmt.Errorf("fleet: a join token must carry the hub's public key: it is what proves the box that answers is the hub")
	}
	e := strings.TrimSpace(t.Endpoint)
	if e == "" {
		return fmt.Errorf("fleet: a join token must say where to dial the hub")
	}
	host, port, err := net.SplitHostPort(e)
	switch {
	case err != nil:
		return fmt.Errorf("fleet: join token: endpoint %q: want host:port: %w", e, err)
	case host == "":
		return fmt.Errorf("fleet: join token: endpoint %q: no host", e)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("fleet: join token: endpoint %q: port %q out of range", e, port)
	}
	for name, s := range map[string]string{"address": t.Address, "peer": t.Peer} {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("fleet: a join token must carry the hub's %s", name)
		}
		if _, err := netip.ParseAddr(s); err != nil {
			return fmt.Errorf("fleet: join token: %s %q: %w", name, s, err)
		}
	}
	if r := strings.TrimSpace(t.Range); r != "" {
		if _, err := netip.ParsePrefix(r); err != nil {
			return fmt.Errorf("fleet: join token: range %q: %w", r, err)
		}
	}
	return nil
}

func (t Ticket) RangePrefix() (netip.Prefix, error) {
	s := strings.TrimSpace(t.Range)
	if s == "" {
		s = FleetRange
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("fleet: join token: range %q: %w", s, err)
	}
	return p.Masked(), nil
}

func (t Ticket) Redacted() Ticket {
	if t.Secret != "" {
		t.Secret = "…"
	}
	return t
}
