package firewall

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

type Manager string

const (
	ManagerNone      Manager = "none"
	ManagerUFW       Manager = "ufw"
	ManagerFirewalld Manager = "firewalld"
	ManagerNftables  Manager = "nftables"
	ManagerIptables  Manager = "iptables"
)

type Verdict string

const (
	VerdictOpen    Verdict = "open"
	VerdictBlocked Verdict = "blocked"
	VerdictUnknown Verdict = "unknown"
)

type PortSpec struct {
	Proto string `json:"proto"`
	Port  int    `json:"port"`
	Why   string `json:"why,omitempty"`
}

func (p PortSpec) String() string { return p.Proto + " " + strconv.Itoa(p.Port) }

type PortCheck struct {
	PortSpec
	Verdict Verdict `json:"verdict"`
	Rule    string  `json:"rule,omitempty"`
}

type Report struct {
	Manager Manager     `json:"manager"`
	Active  bool        `json:"active"`
	Detail  string      `json:"detail,omitempty"`
	Ports   []PortCheck `json:"ports,omitempty"`
	Root    bool        `json:"root"`
}

func (r Report) Blocked() []PortCheck {
	var out []PortCheck
	for _, p := range r.Ports {
		if p.Verdict == VerdictBlocked {
			out = append(out, p)
		}
	}
	return out
}

func (r Report) Says(spec PortSpec) Verdict {
	for _, p := range r.Ports {
		if p.Proto == spec.Proto && p.Port == spec.Port {
			return p.Verdict
		}
	}
	return VerdictUnknown
}

func (r Report) Find(spec PortSpec) (PortCheck, bool) {
	for _, p := range r.Ports {
		if p.Proto == spec.Proto && p.Port == spec.Port {
			return p, true
		}
	}
	return PortCheck{}, false
}

const NotRoot = "cannot tell without root"

func Check(ctx context.Context, run runner.Runner, euid int, want []PortSpec) (Report, error) {
	rep := Report{Manager: ManagerNone, Root: euid == 0, Ports: []PortCheck{}}
	if euid != 0 {
		rep.Detail = NotRoot
		for _, p := range want {
			rep.Ports = append(rep.Ports, PortCheck{PortSpec: p, Verdict: VerdictUnknown, Rule: NotRoot})
		}
		return rep, nil
	}
	if run == nil {
		return rep, fmt.Errorf("read the firewall: no command runner")
	}
	st, err := detect(ctx, run)
	if err != nil {
		return rep, err
	}
	rep.Manager, rep.Active, rep.Detail = st.manager, st.manager != ManagerNone, st.detail
	for _, p := range want {
		verdict, rule := st.says(p)
		rep.Ports = append(rep.Ports, PortCheck{PortSpec: p, Verdict: verdict, Rule: rule})
	}
	return rep, nil
}

func OpenCommand(m Manager, p PortSpec) (cmds []runner.Cmd, printed string, supported bool) {
	spec := strconv.Itoa(p.Port) + "/" + p.Proto
	switch m {
	case ManagerUFW:
		return []runner.Cmd{{Name: "ufw", Args: []string{"allow", spec}}}, "ufw allow " + spec, true
	case ManagerFirewalld:
		return []runner.Cmd{
				{Name: "firewall-cmd", Args: []string{"--permanent", "--add-port=" + spec}},
				{Name: "firewall-cmd", Args: []string{"--reload"}},
			},
			"firewall-cmd --permanent --add-port=" + spec + " && firewall-cmd --reload", true
	case ManagerNftables:
		return nil, fmt.Sprintf("nft add rule inet filter input %s dport %d accept", p.Proto, p.Port), false
	case ManagerIptables:
		return nil, fmt.Sprintf("iptables -I INPUT -p %s --dport %d -j ACCEPT", p.Proto, p.Port), false
	}
	return nil, "", false
}

type state struct {
	manager Manager
	detail  string
	unsure  bool
	decide  func(PortSpec) (Verdict, string)
}

func (s state) says(p PortSpec) (Verdict, string) {
	if s.decide != nil {
		return s.decide(p)
	}
	if s.unsure {
		return VerdictUnknown, ""
	}
	return VerdictOpen, ""
}

type reader func(ctx context.Context, run runner.Runner) (*state, error)

func detect(ctx context.Context, run runner.Runner) (state, error) {
	var notes []string
	unsure := false
	for _, read := range []reader{readUFW, readFirewalld, readNftables, readIptables} {
		st, err := read(ctx, run)
		if err != nil {
			return state{}, err
		}
		if st == nil {
			continue
		}
		if st.manager != ManagerNone {
			st.detail = strings.Join(append(notes, st.detail), "; ")
			return *st, nil
		}
		if st.detail != "" {
			notes = append(notes, st.detail)
		}
		unsure = unsure || st.unsure
	}
	out := state{manager: ManagerNone, unsure: unsure}
	switch {
	case unsure:
		out.detail = strings.Join(append(notes, "what is in charge here cannot be worked out"), "; ")
	case len(notes) > 0:
		out.detail = strings.Join(notes, "; ") + "; nothing is in force"
	default:
		out.detail = "no firewall manager is in charge here"
	}
	return out, nil
}

func have(ctx context.Context, run runner.Runner, tool string) (bool, error) {
	res, err := run.Run(ctx, runner.Cmd{Name: "sh", Args: []string{"-c", "command -v " + tool}})
	if err != nil {
		return false, fmt.Errorf("look for %s: %w", tool, err)
	}
	return res.ExitCode == 0, nil
}

func output(ctx context.Context, run runner.Runner, c runner.Cmd) (string, int, error) {
	res, err := run.Run(ctx, c)
	if err != nil {
		line := c.Name + " " + strings.Join(c.Args, " ")
		return "", 0, fmt.Errorf("run %s: %w", strings.TrimSpace(line), err)
	}
	return res.Stdout + res.Stderr, res.ExitCode, nil
}

func readUFW(ctx context.Context, run runner.Runner) (*state, error) {
	present, err := have(ctx, run, "ufw")
	if err != nil || !present {
		return nil, err
	}
	out, code, err := output(ctx, run, runner.Cmd{Name: "ufw", Args: []string{"status", "verbose"}})
	if err != nil {
		return nil, err
	}
	status := fieldAfter(out, "Status:")
	switch {
	case code != 0 || status == "":
		return &state{manager: ManagerNone, unsure: true,
			detail: "ufw is installed and its status could not be read"}, nil
	case !strings.EqualFold(status, "active"):
		return &state{manager: ManagerNone, detail: "ufw is installed but inactive"}, nil
	}
	rules := parseUFWRules(out)
	fallback := ufwDefaultIncoming(out)
	detail := "ufw is active"
	if fallback != "" {
		detail += ", default " + fallback + " (incoming)"
	}
	return &state{manager: ManagerUFW, detail: detail, decide: func(p PortSpec) (Verdict, string) {
		undecided := ""
		for _, r := range rules {
			if !r.in || r.profile {
				continue
			}
			switch {
			case !r.readable, r.matches(p) && !r.anywhere:
				if undecided == "" {
					undecided = r.line
				}
				continue
			case !r.matches(p):
				continue
			}
			switch r.action {
			case "ALLOW", "LIMIT":
				return VerdictOpen, r.line
			default:
				return VerdictBlocked, r.line
			}
		}
		if undecided != "" {
			return VerdictUnknown, undecided
		}
		switch fallback {
		case "allow":
			return VerdictOpen, "Default: allow (incoming)"
		case "deny", "reject":
			return VerdictBlocked, "Default: " + fallback + " (incoming)"
		}
		return VerdictUnknown, ""
	}}, nil
}

type ufwRule struct {
	line     string
	action   string
	in       bool
	anywhere bool
	readable bool
	profile  bool
	ports    []portRange
}

func (r ufwRule) matches(p PortSpec) bool {
	for _, pr := range r.ports {
		if pr.matches(p) {
			return true
		}
	}
	return false
}

var ufwActions = map[string]bool{"ALLOW": true, "DENY": true, "REJECT": true, "LIMIT": true}

func parseUFWRules(out string) []ufwRule {
	var rules []ufwRule
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasSuffix(fields[0], ":") {
			continue
		}
		at := -1
		for i, f := range fields {
			if ufwActions[f] {
				at = i
				break
			}
		}
		if at < 0 || at == 0 {
			continue
		}
		r := ufwRule{line: strings.Join(fields, " "), action: strings.ToUpper(fields[at]), in: true, readable: true}
		from := fields[at+1:]
		if len(from) > 0 {
			switch strings.ToUpper(from[0]) {
			case "IN":
				from = from[1:]
			case "OUT", "FWD":
				r.in = false
			}
		}
		to := fields[:at]
		if strings.Contains(strings.Join(to, " "), "(v6)") {
			continue
		}
		r.anywhere = len(from) > 0 && ufwAnywhere(from[0])
		for _, t := range to {
			pr, ok := parsePortRange(t)
			if ok {
				r.ports = append(r.ports, pr)
			}
		}
		if len(r.ports) == 0 {
			switch {
			case len(to) == 1 && ufwAnywhere(to[0]):
				r.ports = append(r.ports, portRange{low: 1, high: 65535})
			case ufwAppProfile(to):
				r.profile = true
			default:
				r.readable = false
			}
		}
		rules = append(rules, r)
	}
	return rules
}

func ufwAppProfile(to []string) bool {
	if len(to) == 0 {
		return false
	}
	for _, t := range to {
		for _, c := range t {
			if c >= '0' && c <= '9' || c == '.' || c == ':' || c == '/' {
				return false
			}
		}
	}
	return true
}

func ufwAnywhere(s string) bool {
	switch strings.ToLower(s) {
	case "anywhere", "any", "0.0.0.0/0":
		return true
	}
	return false
}

func ufwDefaultIncoming(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Default:") {
			continue
		}
		for _, part := range strings.Split(strings.TrimPrefix(line, "Default:"), ",") {
			if !strings.Contains(part, "(incoming)") {
				continue
			}
			if fields := strings.Fields(part); len(fields) > 0 {
				return strings.ToLower(fields[0])
			}
		}
	}
	return ""
}

var firewalldServices = map[string]PortSpec{
	"ssh":           {Proto: "tcp", Port: 22},
	"http":          {Proto: "tcp", Port: 80},
	"https":         {Proto: "tcp", Port: 443},
	"dhcpv6-client": {Proto: "udp", Port: 546},
	"mdns":          {Proto: "udp", Port: 5353},
	"cockpit":       {Proto: "tcp", Port: 9090},
}

func readFirewalld(ctx context.Context, run runner.Runner) (*state, error) {
	present, err := have(ctx, run, "firewall-cmd")
	if err != nil || !present {
		return nil, err
	}
	out, _, err := output(ctx, run, runner.Cmd{Name: "firewall-cmd", Args: []string{"--state"}})
	if err != nil {
		return nil, err
	}
	switch got := strings.TrimSpace(out); {
	case got == "running":
	case strings.Contains(got, "not running"):
		return &state{manager: ManagerNone, detail: "firewalld is installed but not running"}, nil
	default:
		return &state{manager: ManagerNone, unsure: true,
			detail: "firewalld is installed and its state could not be read"}, nil
	}
	listed, code, err := output(ctx, run, runner.Cmd{Name: "firewall-cmd", Args: []string{"--list-all"}})
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(listed) == "" {
		return &state{manager: ManagerFirewalld, unsure: true,
			detail: "firewalld is running and its zone could not be read",
			decide: func(PortSpec) (Verdict, string) { return VerdictUnknown, "" }}, nil
	}
	zone := parseFirewalld(listed)
	return &state{manager: ManagerFirewalld, detail: "firewalld is running, zone target " + strOr(zone.target, "default"),
		decide: zone.decide}, nil
}

type firewalldZone struct {
	target    string
	ports     []portRange
	services  []string
	richRules bool
}

func (z firewalldZone) decide(p PortSpec) (Verdict, string) {
	for _, pr := range z.ports {
		if pr.matches(p) {
			return VerdictOpen, "ports: " + pr.String()
		}
	}
	for _, s := range z.services {
		spec, known := firewalldServices[s]
		if !known {
			return VerdictUnknown, "services: " + strings.Join(z.services, " ")
		}
		if spec.Proto == p.Proto && spec.Port == p.Port {
			return VerdictOpen, "services: " + s
		}
	}
	if z.richRules {
		return VerdictUnknown, "rich rules are in force"
	}
	switch strings.ToUpper(z.target) {
	case "ACCEPT":
		return VerdictOpen, "target: ACCEPT"
	case "", "DEFAULT", "DROP", "%%REJECT%%", "REJECT":
		return VerdictBlocked, "target: " + strOr(z.target, "default")
	}
	return VerdictUnknown, "target: " + z.target
}

func parseFirewalld(out string) firewalldZone {
	var z firewalldZone
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "target":
			z.target = value
		case "services":
			z.services = strings.Fields(value)
		case "ports", "source-ports":
			for _, f := range strings.Fields(value) {
				if pr, ok := parsePortRange(f); ok {
					z.ports = append(z.ports, pr)
				}
			}
		case "rich rules":
			z.richRules = value != ""
		}
	}
	return z
}

func readNftables(ctx context.Context, run runner.Runner) (*state, error) {
	present, err := have(ctx, run, "nft")
	if err != nil || !present {
		return nil, err
	}
	out, code, err := output(ctx, run, runner.Cmd{Name: "nft", Args: []string{"list", "ruleset"}})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return &state{manager: ManagerNone, unsure: true,
			detail: "nftables is installed and its ruleset could not be read"}, nil
	}
	if strings.TrimSpace(out) == "" {
		return &state{manager: ManagerNone, detail: "nftables is installed, the ruleset is empty"}, nil
	}
	rs := parseNftables(out)
	detail := "nftables holds a ruleset"
	if len(rs.chains) > 0 {
		detail += fmt.Sprintf(" with %d inet input chain(s)", len(rs.chains))
	} else {
		detail += " this reader does not decide from (no inet input chain)"
	}
	return &state{manager: ManagerNftables, detail: detail, decide: rs.decide}, nil
}

type nftChain struct {
	table  string
	name   string
	policy string
	rules  []nftRule
}

type nftRule struct {
	line     string
	verdict  string
	ports    []portRange
	proto    string
	readable bool
}

type nftRuleset struct {
	chains []nftChain
	unsure bool
}

func (rs nftRuleset) decide(p PortSpec) (Verdict, string) {
	accepted, accepting := "", false
	drops := ""
	for _, c := range rs.chains {
		for _, r := range c.rules {
			if !r.readable {
				return VerdictUnknown, r.line
			}
			if r.proto != p.Proto || !matchesAny(r.ports, p) {
				continue
			}
			switch r.verdict {
			case "drop", "reject":
				return VerdictBlocked, "table " + c.table + " chain " + c.name + ": " + r.line
			case "accept":
				accepted, accepting = "table "+c.table+" chain "+c.name+": "+r.line, true
			}
		}
		if c.policy == "drop" || c.policy == "reject" {
			drops = "table " + c.table + " chain " + c.name + " policy " + c.policy
		}
	}
	switch {
	case accepting && drops != "":
		return VerdictUnknown, accepted + " (with " + drops + ")"
	case accepting:
		return VerdictOpen, accepted
	case drops != "":
		return VerdictBlocked, drops
	case rs.unsure || len(rs.chains) == 0:
		return VerdictUnknown, ""
	}
	return VerdictOpen, ""
}

func parseNftables(out string) nftRuleset {
	var rs nftRuleset
	family, table, chain := "", "", (*nftChain)(nil)
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "table "):
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				family, table = fields[1], fields[2]
			}
			chain = nil
		case strings.HasPrefix(line, "chain "):
			fields := strings.Fields(line)
			name := ""
			if len(fields) >= 2 {
				name = fields[1]
			}
			chain = &nftChain{table: family + " " + table, name: name}
		case line == "}":
			if chain != nil {
				rs.append(family, *chain)
				chain = nil
			}
		case chain != nil && strings.Contains(line, "hook input"):
			chain.policy = strings.Trim(fieldAfter(line, "policy"), ";")
			chain.name += "|input"
		case chain != nil && (strings.Contains(line, "jump ") || strings.Contains(line, "goto ")):
			chain.rules = append(chain.rules, nftRule{line: line})
		case chain != nil:
			if r, ok := parseNftRule(line); ok {
				chain.rules = append(chain.rules, r)
			}
		}
	}
	if chain != nil {
		rs.append(family, *chain)
	}
	return rs
}

func (rs *nftRuleset) append(family string, c nftChain) {
	if !strings.HasSuffix(c.name, "|input") {
		return
	}
	c.name = strings.TrimSuffix(c.name, "|input")
	if family != "inet" {
		rs.unsure = true
		return
	}
	rs.chains = append(rs.chains, c)
}

var nftVerdicts = map[string]bool{"accept": true, "drop": true, "reject": true}

func parseNftRule(line string) (nftRule, bool) {
	if !strings.Contains(line, "dport") {
		return nftRule{}, false
	}
	r := nftRule{line: line}
	fields := strings.Fields(line)
	for i, f := range fields {
		if nftVerdicts[f] {
			r.verdict = f
		}
		if f != "dport" || i == 0 || i+1 >= len(fields) {
			continue
		}
		switch fields[i-1] {
		case "tcp", "udp":
			r.proto = fields[i-1]
		case "th":
			if l4 := fieldAfter(line, "l4proto"); l4 == "tcp" || l4 == "udp" {
				r.proto = l4
			}
		}
		r.ports = parseNftPorts(fields[i+1:])
	}
	r.readable = r.proto != "" && len(r.ports) > 0 && r.verdict != ""
	return r, true
}

func parseNftPorts(fields []string) []portRange {
	var out []portRange
	for _, f := range fields {
		f = strings.Trim(f, "{},")
		if f == "" {
			continue
		}
		if pr, ok := parsePortRange(strings.ReplaceAll(f, "-", ":")); ok {
			out = append(out, pr)
			continue
		}
		break
	}
	return out
}

func readIptables(ctx context.Context, run runner.Runner) (*state, error) {
	present, err := have(ctx, run, "iptables")
	if err != nil || !present {
		return nil, err
	}
	out, code, err := output(ctx, run, runner.Cmd{Name: "iptables", Args: []string{"-S", "INPUT"}})
	if err != nil {
		return nil, err
	}
	policy, rules, unsure := parseIptables(out)
	if code != 0 || policy == "" {
		return &state{manager: ManagerNone, unsure: true,
			detail: "iptables is installed and its INPUT chain could not be read"}, nil
	}
	if policy == "ACCEPT" && len(rules) == 0 && !unsure {
		return &state{manager: ManagerNone, detail: "iptables is installed, its INPUT chain is empty and accepts"}, nil
	}
	return &state{manager: ManagerIptables, detail: "iptables holds an INPUT chain with policy " + policy,
		decide: func(p PortSpec) (Verdict, string) {
			accepted := ""
			for _, r := range rules {
				if r.proto != "" && r.proto != p.Proto {
					continue
				}
				if len(r.ports) > 0 && !matchesAny(r.ports, p) {
					continue
				}
				switch r.verdict {
				case "DROP", "REJECT":
					return VerdictBlocked, r.line
				case "ACCEPT":
					accepted = r.line
				}
			}
			switch {
			case unsure:
				return VerdictUnknown, "the INPUT chain jumps somewhere this reader does not follow"
			case accepted != "" && policy != "ACCEPT":
				return VerdictUnknown, accepted + " (with -P INPUT " + policy + ")"
			case accepted != "":
				return VerdictOpen, accepted
			case policy == "DROP" || policy == "REJECT":
				return VerdictBlocked, "-P INPUT " + policy
			}
			return VerdictOpen, "-P INPUT ACCEPT"
		}}, nil
}

type iptablesRule struct {
	line    string
	proto   string
	verdict string
	ports   []portRange
}

var iptablesTargets = map[string]bool{"ACCEPT": true, "DROP": true, "REJECT": true, "RETURN": true, "LOG": true}

func parseIptables(out string) (policy string, rules []iptablesRule, unsure bool) {
	for _, raw := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 3 {
			continue
		}
		switch fields[0] {
		case "-P":
			if fields[1] == "INPUT" {
				policy = strings.ToUpper(fields[2])
			}
		case "-A":
			if fields[1] != "INPUT" {
				continue
			}
			r := iptablesRule{line: strings.Join(fields, " ")}
			r.proto = strings.ToLower(fieldAfter(r.line, "-p"))
			r.verdict = strings.ToUpper(fieldAfter(r.line, "-j"))
			if pr, ok := parsePortRange(fieldAfter(r.line, "--dport")); ok {
				r.ports = append(r.ports, pr)
			}
			switch {
			case !iptablesTargets[r.verdict]:
				unsure = true
			case r.verdict == "LOG" || r.verdict == "RETURN":
			case len(r.ports) > 0 && (r.proto == "tcp" || r.proto == "udp"):
				rules = append(rules, r)
			case len(fields) == 4:
				rules = append(rules, r)
			}
		}
	}
	return policy, rules, unsure
}

type portRange struct {
	low, high int
	proto     string
}

func (r portRange) String() string {
	s := strconv.Itoa(r.low)
	if r.high != r.low {
		s += ":" + strconv.Itoa(r.high)
	}
	if r.proto != "" {
		s += "/" + r.proto
	}
	return s
}

func (r portRange) matches(p PortSpec) bool {
	if r.proto != "" && r.proto != p.Proto {
		return false
	}
	return p.Port >= r.low && p.Port <= r.high
}

func matchesAny(rs []portRange, p PortSpec) bool {
	for _, r := range rs {
		if p.Port >= r.low && p.Port <= r.high {
			return true
		}
	}
	return false
}

func parsePortRange(s string) (portRange, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return portRange{}, false
	}
	proto := ""
	if ports, p, found := strings.Cut(s, "/"); found {
		switch strings.ToLower(p) {
		case "tcp", "udp":
			proto, s = strings.ToLower(p), ports
		default:
			return portRange{}, false
		}
	}
	low, high, found := strings.Cut(s, ":")
	if !found {
		high = low
	}
	l, err := strconv.Atoi(low)
	if err != nil || l < 1 || l > 65535 {
		return portRange{}, false
	}
	h, err := strconv.Atoi(high)
	if err != nil || h < l || h > 65535 {
		return portRange{}, false
	}
	return portRange{low: l, high: h, proto: proto}, true
}

func fieldAfter(s, key string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		if f == key && i+1 < len(fields) {
			return strings.Trim(fields[i+1], ";")
		}
	}
	return ""
}

func strOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
