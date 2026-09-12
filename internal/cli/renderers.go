package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/state"
)

func init() {

	registerRenderer("status", func(a *app) Renderer {
		return viewRenderer(func(r *statusResult) *ui.View { return statusResultView(r, time.Now()) })
	})
	registerRenderer("machine show", func(a *app) Renderer {

		detail := viewRenderer(func(d *api.MachineDetail) *ui.View {
			return machineDetailView(d, time.Now())
		})
		record := viewRenderer(func(r *machine.Record) *ui.View { return machineView(r) })
		return func(w io.Writer, result json.RawMessage) error {
			var probe struct {
				Machine json.RawMessage `json:"machine"`
			}
			if err := json.Unmarshal(result, &probe); err != nil {
				return fmt.Errorf("decode the result: %w", err)
			}
			if len(probe.Machine) > 0 {
				return detail(w, result)
			}
			return record(w, result)
		}
	})

	registerRenderer("machine list", func(a *app) Renderer {
		return viewRenderer(func(ms []fleet.Machine) *ui.View { return machinesView(ms, time.Now()) })
	})
	registerRenderer("machine token", func(a *app) Renderer {
		return viewRenderer(func(r *api.MachineTokenResult) *ui.View { return machineTokenView(r) })
	})
	registerRenderer("machine remove", func(a *app) Renderer {
		return viewRenderer(func(r removed) *ui.View { return removedView("machine", r) })
	})
	registerRenderer("env handoff", func(a *app) Renderer {
		return viewRenderer(func(e *env.Env) *ui.View { return handoffView(e) })
	})
	registerRenderer("key list", func(a *app) Renderer {
		return viewRenderer(func(keys []state.Key) *ui.View { return keysView(keys) })
	})
	registerRenderer("peer list", func(a *app) Renderer {
		return viewRenderer(func(peers []state.Peer) *ui.View { return peersView(peers) })
	})
	registerRenderer("key add", func(a *app) Renderer {
		return viewRenderer(func(k *state.Key) *ui.View { return keyAddedView(k) })
	})
	registerRenderer("key remove", func(a *app) Renderer {
		return viewRenderer(func(r removed) *ui.View { return removedView("key", r) })
	})
	registerRenderer("peer add", func(a *app) Renderer {
		return viewRenderer(func(p *state.Peer) *ui.View { return peerAddedView(p) })
	})
	registerRenderer("peer remove", func(a *app) Renderer {
		return viewRenderer(func(r removed) *ui.View { return removedView("peer", r) })
	})

	registerRenderer("app list", func(a *app) Renderer {
		return viewRenderer(func(apps []api.AppInfo) *ui.View { return appsView(apps) })
	})
	registerRenderer("env list", func(a *app) Renderer {
		return viewRenderer(func(envs []env.Env) *ui.View { return envsView(envs) })
	})
	registerRenderer("env show", func(a *app) Renderer {
		return viewRenderer(func(d *api.EnvDetail) *ui.View { return envDetailView(d) })
	})
	registerRenderer("env create", func(a *app) Renderer {
		return viewRenderer(func(e *env.Env) *ui.View { return envRecordView(e, nil) })
	})
	registerRenderer("env destroy", func(a *app) Renderer {
		return viewRenderer(func(d destroyed) *ui.View { return destroyedView(d) })
	})

	registerRenderer("up", func(a *app) Renderer {
		return viewRenderer(func(r *api.UpResult) *ui.View { return upView(r) })
	})
	registerRenderer("down", func(a *app) Renderer {

		name := a.render.env
		return viewRenderer(func(r *api.DownResult) *ui.View { return downView(name, r) })
	})
	registerRenderer("config show", func(a *app) Renderer {
		return viewRenderer(func(c *api.EffectiveConfig) *ui.View { return configView(c) })
	})

	registerRenderer("edge status", func(a *app) Renderer {
		return viewRenderer(func(st *edge.Status) *ui.View { return edgeStatusView(st) })
	})
	registerRenderer("edge ca", func(a *app) Renderer {

		return func(w io.Writer, result json.RawMessage) error {
			var ca certs.CA
			if err := json.Unmarshal(result, &ca); err != nil {
				return fmt.Errorf("decode the result: %w", err)
			}
			return a.writeCA(w, &ca)
		}
	})
	registerRenderer("env expose", func(a *app) Renderer {
		return viewRenderer(func(r *api.ExposeResult) *ui.View { return exposeView(r, "exposed") })
	})
	registerRenderer("env unexpose", func(a *app) Renderer {
		return viewRenderer(func(r *api.ExposeResult) *ui.View {
			return exposeView(r, "no longer exposed")
		})
	})
}
