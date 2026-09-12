package vpn

import "strings"

const Domain = "internal"

const Suffix = "." + Domain

func EnvHost(app, env string) string {
	return join(env, app, Domain)
}

func ServiceHost(app, env, name string) string {
	return join(name, env, app, Domain)
}

func MachineHost(machine string) string {
	return join(machine, Domain)
}

func EnvHosts(app, env string, names []string) []string {
	out := make([]string, 0, len(names)+1)
	out = append(out, EnvHost(app, env))
	for _, n := range names {
		out = append(out, ServiceHost(app, env, n))
	}
	return out
}

func Normalize(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

func IsInternal(name string) bool {
	n := Normalize(name)
	return n != "" && n != Domain && strings.HasSuffix(n, Suffix)
}

func join(parts ...string) string { return strings.Join(parts, ".") }
