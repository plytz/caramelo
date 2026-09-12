package edge

import "time"

const AccessLogPrefix = "access"

type AccessLog struct {
	At time.Time `json:"at"`

	Host   string `json:"host"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`

	Proto string `json:"proto,omitempty"`

	Status int `json:"status,omitempty"`

	Bytes int64 `json:"bytes,omitempty"`

	Duration time.Duration `json:"duration,omitempty"`

	Client string `json:"client,omitempty"`

	App     string `json:"app,omitempty"`
	Env     string `json:"env,omitempty"`
	Service string `json:"service,omitempty"`
	Replica int    `json:"replica,omitempty"`

	Target string `json:"target,omitempty"`

	WebSocket bool `json:"websocket,omitempty"`

	Error string `json:"error,omitempty"`
}
