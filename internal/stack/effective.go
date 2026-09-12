package stack

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/config"
)

type Source string

const (
	SourceFile Source = "file"

	SourceDetected Source = "detected"

	SourceDefault Source = "default"
)

type Setting struct {
	Key string `json:"key"`

	Value string `json:"value"`

	Source Source `json:"source"`

	Evidence string `json:"evidence,omitempty"`
}

type Effective struct {
	Stack string `json:"stack,omitempty"`

	Config *config.App `json:"config"`

	Settings []Setting `json:"settings,omitempty"`

	Cache string `json:"cache,omitempty"`
}

const PortReference = "${port}"

func Merge(file *config.App, g *Guess) *Effective {
	eff := &Effective{Config: copyApp(file)}
	if g != nil {
		eff.Stack = g.Stack
		eff.Cache = g.Cache
	}
	app := eff.Config

	if len(app.Services) == 0 && g != nil {
		app.Services = []config.Service{{Name: config.DefaultServiceName}}
	}
	for i := range app.Services {
		eff.Settings = append(eff.Settings, mergeService(&app.Services[i], g)...)
	}

	switch {
	case app.Test != "":
		eff.Settings = append(eff.Settings, Setting{Key: "test", Value: app.Test, Source: SourceFile})
	case g != nil && g.Test.Value != "":
		app.Test = g.Test.Value
		eff.Settings = append(eff.Settings, Setting{
			Key: "test", Value: app.Test, Source: SourceDetected, Evidence: g.Test.Evidence,
		})
	}

	eff.Settings = append(eff.Settings, depSettings(app.Deps)...)
	eff.Settings = append(eff.Settings, fleetSettings(app)...)
	return eff
}

func fleetSettings(app *config.App) []Setting {
	fs := app.FleetSettings()
	out := make([]Setting, 0, len(fs))
	for _, f := range fs {
		s := Setting{Key: f.Key, Value: f.Value, Source: SourceDefault, Evidence: f.Evidence}
		if f.FromFile {
			s.Source = SourceFile
		}
		out = append(out, s)
	}
	return out
}

func mergeService(s *config.Service, g *Guess) []Setting {
	key := "services." + s.Name + "."
	var out []Setting

	switch {
	case s.Build != nil:
		out = append(out, Setting{Key: key + "build", Value: renderBuild(s.Build), Source: SourceFile})
	case s.Image != "":
		out = append(out, Setting{Key: key + "image", Value: s.Image, Source: SourceFile})
	case g != nil && g.Build.Value != "":
		s.Build = &config.Build{Context: g.Build.Value, Dockerfile: g.Dockerfile.Value}
		out = append(out, Setting{
			Key: key + "build", Value: renderBuild(s.Build), Source: SourceDetected, Evidence: g.Build.Evidence,
		})
	case g != nil && g.Image.Value != "":
		s.Image = g.Image.Value
		out = append(out, Setting{
			Key: key + "image", Value: s.Image, Source: SourceDetected, Evidence: g.Image.Evidence,
		})
	}

	switch {
	case s.Build != nil:
	case s.Install != "":
		out = append(out, Setting{Key: key + "install", Value: s.Install, Source: SourceFile})
	case g != nil && g.Install.Value != "":
		s.Install = g.Install.Value
		out = append(out, Setting{
			Key: key + "install", Value: s.Install, Source: SourceDetected, Evidence: g.Install.Evidence,
		})
	}

	switch {
	case s.Run != "":
		out = append(out, Setting{Key: key + "run", Value: s.Run, Source: SourceFile})
	case g != nil:
		s.Run = g.Run.Value
		out = append(out, Setting{
			Key: key + "run", Value: s.Run, Source: SourceDetected, Evidence: g.Run.Evidence,
		})
	default:
		out = append(out, Setting{
			Key: key + "run", Source: SourceDefault,
			Evidence: "nothing detected: caramelo.yaml must say how to run this service",
		})
	}

	switch {
	case s.Port == config.PortNone:
		out = append(out, Setting{Key: key + "port", Value: "none", Source: SourceFile})
	case s.Port > 0:
		out = append(out, Setting{Key: key + "port", Value: strconv.Itoa(s.Port), Source: SourceFile})
	default:
		out = append(out, Setting{
			Key: key + "port", Value: PortReference, Source: SourceDefault,
			Evidence: "the environment's own port",
		})
	}

	if s.Protocol == config.ProtocolUDP {
		out = append(out, Setting{Key: key + "protocol", Value: string(config.ProtocolUDP), Source: SourceFile})
	} else {
		s.Protocol = config.ProtocolTCP
		out = append(out, Setting{Key: key + "protocol", Value: string(config.ProtocolTCP), Source: SourceDefault})
	}

	out = append(out, healthSetting(key, s))
	return out
}

func healthSetting(key string, s *config.Service) Setting {
	switch {
	case s.Health != nil && s.Health.Path != "":
		return Setting{Key: key + "health", Value: "GET " + s.Health.Path, Source: SourceFile}
	case s.Health != nil && len(s.Health.Command) > 0:
		return Setting{Key: key + "health", Value: strings.Join(s.Health.Command, " "), Source: SourceFile}
	case s.Port == config.PortNone:
		return Setting{
			Key: key + "health", Value: "none", Source: SourceDefault,
			Evidence: "the service has no port",
		}
	case s.Protocol == config.ProtocolUDP:
		return Setting{
			Key: key + "health", Value: "none", Source: SourceDefault,
			Evidence: "a UDP service has nothing to connect to",
		}
	}
	return Setting{
		Key: key + "health", Value: "tcp connect", Source: SourceDefault,
		Evidence: "on the service's own port",
	}
}

func depSettings(deps []config.Dep) []Setting {
	out := make([]Setting, 0, 2*len(deps))
	for _, d := range deps {
		key := "deps." + d.Name + "."
		out = append(out, Setting{Key: key + "image", Value: d.Image, Source: SourceFile})
		port := Setting{Key: key + "port", Value: strconv.Itoa(d.Port), Source: SourceFile}
		if known, ok := config.Lookup(d.Image); ok && known.Port == d.Port {
			port.Source = SourceDefault
			port.Evidence = fmt.Sprintf("%s is a known image", config.ImageName(d.Image))
		}
		out = append(out, port)
	}
	return out
}

func renderBuild(b *config.Build) string {
	ctx := b.Context
	if ctx == "" {
		ctx = "."
	}
	if b.Dockerfile == "" {
		return ctx
	}
	return ctx + " (" + b.Dockerfile + ")"
}

func copyApp(a *config.App) *config.App {
	if a == nil {
		return &config.App{}
	}
	out := *a
	out.Deps = append([]config.Dep(nil), a.Deps...)
	out.Services = append([]config.Service(nil), a.Services...)
	return &out
}
