package env

import (
	"strconv"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
)

func ReplicaContainerName(app, envName, service string, replica int) string {
	return ServiceContainerName(app, envName, service) + "-" + strconv.Itoa(replica)
}

type ReplicaState string

const (
	ReplicaStarting ReplicaState = "starting"

	ReplicaHealthy ReplicaState = "healthy"

	ReplicaActive ReplicaState = "active"

	ReplicaDraining ReplicaState = "draining"

	ReplicaStopped ReplicaState = "stopped"

	ReplicaFailed ReplicaState = "failed"

	ReplicaHeld ReplicaState = "held"

	ReplicaUnhealthy ReplicaState = "unhealthy"
)

var ReplicaStates = []ReplicaState{
	ReplicaStarting, ReplicaHealthy, ReplicaActive, ReplicaDraining, ReplicaStopped, ReplicaFailed,
	ReplicaHeld, ReplicaUnhealthy,
}

func (s ReplicaState) Valid() bool {
	for _, k := range ReplicaStates {
		if k == s {
			return true
		}
	}
	return false
}

func (s ReplicaState) String() string { return string(s) }

func (s ReplicaState) TargetState() edge.TargetState {
	switch s {
	case ReplicaActive:
		return edge.TargetActive
	case ReplicaDraining:
		return edge.TargetDraining
	case ReplicaStopped, ReplicaFailed:
		return edge.TargetStopped
	case ReplicaHeld:
		return edge.TargetHeld
	case ReplicaUnhealthy:
		return edge.TargetUnhealthy
	default:
		return edge.TargetStarting
	}
}

type Replica struct {
	Service string `json:"service"`

	Index int `json:"index"`

	Container string `json:"container"`
	ID        string `json:"id,omitempty"`

	Port int `json:"port,omitempty"`

	State ReplicaState `json:"state"`

	Status ServiceStatus `json:"status,omitempty"`
	Health HealthStatus  `json:"health,omitempty"`

	Inflight int `json:"inflight"`

	Since time.Time `json:"since,omitempty"`

	Detail string `json:"detail,omitempty"`

	Restarts int `json:"restarts,omitempty"`
}

type RolloutStepName string

const (
	StepStart RolloutStepName = "start"

	StepHealth RolloutStepName = "health"

	StepProbe RolloutStepName = "probe"

	StepFlip RolloutStepName = "flip"

	StepDrain RolloutStepName = "drain"

	StepStop RolloutStepName = "stop"

	StepRoute RolloutStepName = "route"
)

type StepStatus string

const (
	StepStarted StepStatus = "started"
	StepOK      StepStatus = "ok"
	StepFailed  StepStatus = "failed"

	StepSkipped StepStatus = "skipped"
)

type RolloutStep struct {
	Service string `json:"service"`
	Replica int    `json:"replica,omitempty"`

	Step RolloutStepName `json:"step"`

	Status StepStatus `json:"status"`

	State ReplicaState `json:"state,omitempty"`

	Host string `json:"host,omitempty"`

	Inflight int `json:"inflight,omitempty"`

	Detail string `json:"detail,omitempty"`

	At       time.Time     `json:"at"`
	Duration time.Duration `json:"duration,omitempty"`
}

type Rollout struct {
	Service string `json:"service"`

	Host string `json:"host,omitempty"`

	URL string `json:"url,omitempty"`

	Steps []RolloutStep `json:"steps,omitempty"`

	Replicas []Replica `json:"replicas,omitempty"`

	Failed *RolloutStep `json:"failed,omitempty"`
}

func (r *Rollout) Add(s RolloutStep) *RolloutStep {
	if s.Service == "" {
		s.Service = r.Service
	}
	r.Steps = append(r.Steps, s)
	step := &r.Steps[len(r.Steps)-1]
	if step.Status == StepFailed && r.Failed == nil {
		r.Failed = step
	}
	return step
}

type ExposeRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Service string `json:"service,omitempty"`

	Host string `json:"host,omitempty"`

	Via config.Via `json:"via,omitempty"`
}

type UnexposeRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Service string `json:"service,omitempty"`

	Host string `json:"host,omitempty"`

	Force bool `json:"force,omitempty"`
}
