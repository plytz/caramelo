package fleet

import (
	"fmt"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/machine"
)

type Demand struct {
	App string `json:"app"`
	Env string `json:"env"`

	MemoryBytes int64 `json:"memory_bytes,omitempty"`

	CPU float64 `json:"cpu,omitempty"`

	Arch string `json:"arch,omitempty"`
}

type Preference struct {
	Pin string `json:"pin,omitempty"`

	PinnedByFile bool `json:"pinned_by_file,omitempty"`

	Placement config.Placement `json:"placement,omitempty"`
}

type Candidate struct {
	Machine Machine `json:"machine"`

	Gauge *machine.Record `json:"gauge,omitempty"`

	Committed int64 `json:"committed,omitempty"`
}

type Consideration struct {
	Machine string `json:"machine"`

	FreeBytes int64 `json:"free_bytes"`

	Envs int `json:"envs"`

	Fits   bool   `json:"fits"`
	Reason string `json:"reason,omitempty"`
}

type Decision struct {
	Machine string `json:"machine,omitempty"`

	Why string `json:"why,omitempty"`

	Considered []Consideration `json:"considered,omitempty"`
}

var ErrNoMachine = fmt.Errorf("no machine in the fleet can hold this environment")

func (c Candidate) Free() (int64, bool) {
	if c.Gauge == nil {
		return 0, false
	}
	free := c.Gauge.Memory.AvailableBytes - c.Gauge.Reserved.MemoryBytes - c.Committed
	if free < 0 {
		free = 0
	}
	return free, true
}
