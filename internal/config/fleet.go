package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const MachineHub = "hub"

type Placement string

const (
	PlacementAuto Placement = "auto"

	PlacementHub Placement = "hub"
)

var Placements = []Placement{PlacementAuto, PlacementHub}

func ParsePlacement(s string) (Placement, error) {
	switch p := Placement(strings.ToLower(strings.TrimSpace(s))); p {
	case "", PlacementAuto:
		return PlacementAuto, nil
	case PlacementHub:
		return PlacementHub, nil
	default:
		return "", fmt.Errorf("unknown placement %q: want %s or %s", s, PlacementAuto, PlacementHub)
	}
}

func (p Placement) String() string {
	if p == "" {
		return string(PlacementAuto)
	}
	return string(p)
}

func (a *App) PlacementOf() Placement {
	if a == nil || a.Placement == "" {
		return PlacementAuto
	}
	return a.Placement
}

type Via string

const (
	ViaNode Via = "node"

	ViaHub Via = "hub"
)

var Vias = []Via{ViaNode, ViaHub}

func ParseVia(s string) (Via, error) {
	switch v := Via(strings.ToLower(strings.TrimSpace(s))); v {
	case "", ViaNode:
		return ViaNode, nil
	case ViaHub:
		return ViaHub, nil
	default:
		return "", fmt.Errorf("unknown via %q: want %s or %s", s, ViaNode, ViaHub)
	}
}

func (v Via) String() string {
	if v == "" {
		return string(ViaNode)
	}
	return string(v)
}

func (a *App) ViaOf(env string) Via {
	o, ok := a.Override(env)
	if !ok || o.Via == "" {
		return ViaNode
	}
	return o.Via
}

func (a *App) MachineOf(env string) string {
	o, ok := a.Override(env)
	if !ok {
		return ""
	}
	return o.Machine
}

type FleetSetting struct {
	Key string `json:"key"`

	Value string `json:"value"`

	FromFile bool `json:"from_file"`

	Evidence string `json:"evidence,omitempty"`
}

func (a *App) FleetSettings() []FleetSetting {
	if a == nil {
		return nil
	}
	out := []FleetSetting{{
		Key:      "placement",
		Value:    a.PlacementOf().String(),
		FromFile: a.Placement != "",
	}}
	if !out[0].FromFile {
		out[0].Evidence = "the machine with the most room; the hub is a candidate like any other"
	}
	for _, name := range sortedEnvNames(a.Envs) {
		o := a.Envs[name]
		key := "envs." + name + "."
		if o.Machine != "" {
			evidence := ""
			if o.Machine == MachineHub {
				evidence = "the hub, whichever machine that is"
			}
			out = append(out, FleetSetting{
				Key: key + "machine", Value: o.Machine, FromFile: true, Evidence: evidence,
			})
		}
		if o.Via != "" {
			evidence := "this environment's own machine serves its public names"
			if o.Via == ViaHub {
				evidence = "the hub's edge serves the names and forwards through the tunnel"
			}
			out = append(out, FleetSetting{
				Key: key + "via", Value: o.Via.String(), FromFile: true, Evidence: evidence,
			})
		}
	}
	return out
}

func parsePlacementNode(node *yaml.Node, path, key string) (Placement, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	p, err := ParsePlacement(s)
	if err != nil {
		return "", keyErr(path, key, "%v", err)
	}
	return p, nil
}

func parseViaNode(node *yaml.Node, path, key string) (Via, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	v, err := ParseVia(s)
	if err != nil {
		return "", keyErr(path, key, "%v", err)
	}
	return v, nil
}

func parseMachineNode(node *yaml.Node, path, key string) (string, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(s)
	if name != "" && !validSlug(name) {
		return "", keyErr(path, key, "invalid machine name %q: use lowercase letters, digits and dashes, "+
			"starting with a letter or a digit, at most %d characters", name, MaxSlugLen)
	}
	return name, nil
}

func (a *App) validateFleet() error {
	if a.Placement != "" {
		if _, err := ParsePlacement(string(a.Placement)); err != nil {
			return keyErr(a.Path, "placement", "%v", err)
		}
	}
	for _, name := range sortedEnvNames(a.Envs) {
		o := a.Envs[name]
		key := "envs." + name
		if o.Machine != "" && !validSlug(o.Machine) {
			return keyErr(a.Path, key+".machine", "invalid machine name %q: use lowercase letters, "+
				"digits and dashes, starting with a letter or a digit, at most %d characters",
				o.Machine, MaxSlugLen)
		}
		if o.Via != "" {
			if _, err := ParseVia(string(o.Via)); err != nil {
				return keyErr(a.Path, key+".via", "%v", err)
			}
		}
	}
	return nil
}
