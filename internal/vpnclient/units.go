package vpnclient

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpn"
)

const (
	UnitName = "caramelo-vpn.service"

	LaunchdLabel = "land.caramelo.vpn"

	DefaultInterface = "caramelo0"

	ResolverFile = "/etc/resolver/" + vpn.Domain

	PrivilegedBinary = "/usr/local/lib/caramelo/caramelo-vpn"

	PrivilegedMode = "0750"

	PolkitRuleFile = "/etc/polkit-1/rules.d/50-caramelo-vpn.rules"
)

func ServiceCommand(binary, machine, iface string) []string {
	argv := []string{binary, "vpn", "service", "--machine", machine}
	if iface != "" && iface != DefaultInterface {
		argv = append(argv, "--interface", iface)
	}
	return argv
}

func SystemdUnit(binary, machine, iface string) string {
	var b strings.Builder
	b.WriteString("# Installed by 'caramelo vpn install'. Remove it with 'caramelo vpn uninstall'.\n")
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=Caramelo tunnel to %s\n", machine)
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoteAll(ServiceCommand(binary, machine, iface)), " "))

	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2\n")

	b.WriteString("NoNewPrivileges=false\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

func LaunchdPlist(binary, machine, iface string) string {
	var args strings.Builder
	for _, a := range ServiceCommand(binary, machine, iface) {
		fmt.Fprintf(&args, "    <string>%s</string>\n", xmlEscape(a))
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by 'caramelo vpn install'. Remove it with 'caramelo vpn uninstall'. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + LaunchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
` + strings.TrimRight(args.String(), "\n") + `
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`
}

func PolkitRule(user string) string {
	var b strings.Builder
	b.WriteString("// Installed by 'caramelo vpn install'. Removed by 'caramelo vpn uninstall'.\n")
	b.WriteString("// The Caramelo tunnel runs unprivileged, and systemd-resolved's per-link DNS\n")
	b.WriteString("// configuration goes through polkit; this lets that one user point the\n")
	b.WriteString("// .internal domain at their machine's resolver.\n")
	b.WriteString("polkit.addRule(function(action, subject) {\n")
	fmt.Fprintf(&b, "    if (subject.user == %s &&\n", jsString(user))
	b.WriteString("        (action.id == \"org.freedesktop.resolve1.set-dns-servers\" ||\n")
	b.WriteString("         action.id == \"org.freedesktop.resolve1.set-domains\" ||\n")
	b.WriteString("         action.id == \"org.freedesktop.resolve1.revert\")) {\n")
	b.WriteString("        return polkit.Result.YES;\n")
	b.WriteString("    }\n")
	b.WriteString("});\n")
	return b.String()
}

func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func ResolverEntry(resolver string) string {
	host, port := resolver, ""
	if h, p, err := splitHostPort(resolver); err == nil {
		host, port = h, p
	}
	var b strings.Builder
	b.WriteString("# Installed by 'caramelo vpn install': the .internal domain, and nothing else,\n")
	b.WriteString("# is resolved inside the Caramelo tunnel.\n")
	fmt.Fprintf(&b, "domain %s\n", vpn.Domain)
	fmt.Fprintf(&b, "nameserver %s\n", host)
	if port != "" && port != "53" {
		fmt.Fprintf(&b, "port %s\n", port)
	}
	return b.String()
}

func ResolvectlCommands(iface, resolver string) [][]string {
	host := resolver
	if h, _, err := splitHostPort(resolver); err == nil {
		host = h
	}
	return [][]string{
		{"resolvectl", "dns", iface, host},
		{"resolvectl", "domain", iface, "~" + vpn.Domain},
	}
}

func SystemdUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", UnitName)
}

func LaunchdPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

func quoteAll(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = remote.Quote(a)
	}
	return out
}

func splitHostPort(s string) (host, port string, err error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", s)
	}
	return s[:i], s[i+1:], nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
