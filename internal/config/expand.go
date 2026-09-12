package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type ExpandContext struct {
	App string

	Env string

	Port int

	Deps map[string]DepAddr

	Services map[string]DepAddr

	View View `json:"view,omitempty"`

	secrets Secrets
}

type SecretFunc func(name string) (value string, ok bool)

type DepPasswordFunc func(dep string) (value string, ok bool)

type Secrets struct {
	Lookup      SecretFunc
	DepPassword DepPasswordFunc
}

func (s Secrets) Empty() bool { return s.Lookup == nil && s.DepPassword == nil }

func (c ExpandContext) WithSecrets(s Secrets) ExpandContext {
	c.secrets = s
	return c
}

func DepPasswordSecret(dep string) string {
	up := strings.ToUpper(strings.TrimSpace(dep))
	up = strings.ReplaceAll(up, "-", "_")
	return up + "_PASSWORD"
}

type DepAddr struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func Expand(vars map[string]string, ctx ExpandContext) (map[string]string, error) {
	if vars == nil {
		return nil, nil
	}
	out := make(map[string]string, len(vars))
	for _, k := range sortedKeys(vars) {
		v, err := ExpandString(vars[k], ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

func ExpandString(s string, ctx ExpandContext) (string, error) {
	return mapRefs(s, func(ref string) (string, bool, error) {
		v, ok, err := ctx.lookup(ref)
		switch {
		case err != nil:
			return "", false, err
		case !ok:
			return "", false, fmt.Errorf("unknown reference %q", "${"+ref+"}")
		}
		return v, true, nil
	})
}

func mapRefs(s string, f func(ref string) (string, bool, error)) (string, error) {
	if !strings.ContainsRune(s, '$') {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteByte('$')
			i += 2
		case i+1 < len(s) && s[i+1] == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("%w %q", errUnterminated, s[i:])
			}
			ref := s[i+2 : i+2+end]
			v, ok, err := f(ref)
			switch {
			case err != nil:
				return "", err
			case !ok:

				b.WriteString(s[i : i+end+3])
			default:
				b.WriteString(v)
			}
			i += end + 3
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}

var errUnterminated = errors.New("unterminated reference")

func ExpandSecretsOnly(s string, deps []Dep, sec Secrets) string {
	if sec.Empty() {
		return s
	}
	declared := make(map[string]bool, len(deps))
	for _, d := range deps {
		declared[d.Name] = true
	}
	out, err := mapRefs(s, func(ref string) (string, bool, error) {
		if name, ok := strings.CutPrefix(ref, "secrets."); ok {
			if !varNameRe.MatchString(name) || sec.Lookup == nil {
				return "", false, nil
			}
			v, ok := sec.Lookup(name)
			return v, ok, nil
		}
		rest, ok := strings.CutPrefix(ref, "deps.")
		if !ok {
			return "", false, nil
		}
		name, field, ok := strings.Cut(rest, ".")
		if !ok || field != "password" || !declared[name] || sec.DepPassword == nil {
			return "", false, nil
		}
		v, ok := sec.DepPassword(name)
		return v, ok, nil
	})
	if err != nil {
		return s
	}
	return out
}

func (a *App) WithSecretsExpanded(sec Secrets) *App {
	if a == nil || sec.Empty() {
		return a
	}
	out := a.clone()
	expand := func(v string) string { return ExpandSecretsOnly(v, a.Deps, sec) }
	out.Env = mapValues(out.Env, expand)
	for i := range out.Deps {
		out.Deps[i].Env = mapValues(out.Deps[i].Env, expand)
	}
	for i := range out.Services {
		out.Services[i].Env = mapValues(out.Services[i].Env, expand)
	}
	return out
}

func mapValues(in map[string]string, f func(string) string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = f(v)
	}
	return out
}

func (a *App) Expanded(ctx ExpandContext) (*App, error) {
	if a == nil {
		return nil, nil
	}
	out := a.clone()
	vars, err := Expand(a.Env, ctx)
	if err != nil {
		return nil, keyErr(a.Path, "env", "%v", err)
	}
	out.Env = vars
	for i := range out.Deps {
		if out.Deps[i].Env, err = Expand(a.Deps[i].Env, ctx); err != nil {
			return nil, keyErr(a.Path, "deps."+a.Deps[i].Name+".env", "%v", err)
		}
	}
	for i := range out.Services {
		if out.Services[i].Env, err = Expand(a.Services[i].Env, ctx); err != nil {
			return nil, keyErr(a.Path, "services."+a.Services[i].Name+".env", "%v", err)
		}
	}
	return out, nil
}

func (a *App) clone() *App {
	out := *a
	if a.Deploy != nil {
		d := *a.Deploy
		out.Deploy = &d
	}
	out.Envs = cloneOverrides(a.Envs)
	out.Env = mapValues(a.Env, func(v string) string { return v })
	out.Deps = make([]Dep, len(a.Deps))
	for i, d := range a.Deps {
		d.Ready = append([]string(nil), d.Ready...)
		d.Env = mapValues(d.Env, func(v string) string { return v })
		out.Deps[i] = d
	}
	out.Services = make([]Service, len(a.Services))
	for i, s := range a.Services {
		if s.Build != nil {
			b := *s.Build
			s.Build = &b
		}
		if s.Health != nil {
			h := *s.Health
			h.Command = append([]string(nil), s.Health.Command...)
			s.Health = &h
		}
		if s.Resources != nil {
			r := *s.Resources
			s.Resources = &r
		}
		s.Env = mapValues(s.Env, func(v string) string { return v })
		out.Services[i] = s
	}
	if len(out.Services) == 0 {
		out.Services = nil
	}
	return &out
}

func cloneOverrides(in map[string]EnvOverride) map[string]EnvOverride {
	if in == nil {
		return nil
	}
	out := make(map[string]EnvOverride, len(in))
	for name, o := range in {
		o.Hosts = append([]string(nil), o.Hosts...)
		if o.Deploy != nil {
			d := *o.Deploy
			o.Deploy = &d
		}
		if o.Services != nil {
			svcs := make(map[string]ServiceOverride, len(o.Services))
			for n, so := range o.Services {
				if so.Resources != nil {
					r := *so.Resources
					so.Resources = &r
				}
				svcs[n] = so
			}
			o.Services = svcs
		}
		out[name] = o
	}
	return out
}

func (c ExpandContext) lookup(ref string) (string, bool, error) {
	switch ref {
	case "port":
		return strconv.Itoa(c.Port), true, nil
	case "app.name":
		return c.App, true, nil
	case "env.name":
		return c.Env, true, nil
	}
	if name, ok := strings.CutPrefix(ref, "secrets."); ok {
		return c.lookupSecret(name)
	}
	if rest, ok := strings.CutPrefix(ref, "deps."); ok {
		if name, field, ok := strings.Cut(rest, "."); ok && field == "password" {
			return c.lookupDepPassword(name)
		}
		v, ok := lookupAddr(c.Deps, rest)
		return v, ok, nil
	}
	if rest, ok := strings.CutPrefix(ref, "services."); ok {
		v, ok := lookupAddr(c.Services, rest)
		return v, ok, nil
	}
	return "", false, nil
}

func (c ExpandContext) lookupSecret(name string) (string, bool, error) {
	if name == "" || !varNameRe.MatchString(name) {
		return "", false, nil
	}
	if c.secrets.Lookup == nil {
		return "", true, nil
	}
	v, ok := c.secrets.Lookup(name)
	if !ok {
		return "", false, fmt.Errorf("unknown secret %q: set it with `caramelo secrets set %s=…`",
			"${secrets."+name+"}", name)
	}
	return v, true, nil
}

func (c ExpandContext) lookupDepPassword(dep string) (string, bool, error) {
	if _, ok := c.Deps[dep]; !ok {
		return "", false, nil
	}
	if c.secrets.DepPassword == nil {
		return "", true, nil
	}
	v, ok := c.secrets.DepPassword(dep)
	if !ok {
		return "", false, fmt.Errorf("dependency %q has no password: set it with `caramelo secrets set %s=…`",
			dep, DepPasswordSecret(dep))
	}
	return v, true, nil
}

func lookupAddr(addrs map[string]DepAddr, rest string) (string, bool) {
	name, field, ok := strings.Cut(rest, ".")
	if !ok {
		return "", false
	}
	addr, ok := addrs[name]
	if !ok {
		return "", false
	}
	switch field {
	case "host":
		return addr.Host, true
	case "port":
		return strconv.Itoa(addr.Port), true
	}
	return "", false
}
