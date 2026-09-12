package stack

const (
	Docker = "docker"

	Go = "go"

	Node = "node"

	Rails = "rails"

	Python = "python"
)

type Field struct {
	Value string `json:"value,omitempty"`

	Evidence string `json:"evidence,omitempty"`
}

type Guess struct {
	Stack string `json:"stack"`

	Image Field `json:"image,omitempty"`

	Build Field `json:"build,omitempty"`

	Dockerfile Field `json:"dockerfile,omitempty"`

	Install Field `json:"install,omitempty"`

	Run Field `json:"run,omitempty"`

	Test Field `json:"test,omitempty"`

	Cache string `json:"cache,omitempty"`
}
