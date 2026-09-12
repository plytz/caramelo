package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

type Printer struct {
	Out  io.Writer
	Err  io.Writer
	JSON bool
}

func (p *Printer) Result(v any, human func(w io.Writer) error) error {
	if p.JSON {
		enc := json.NewEncoder(p.Out)
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encoding JSON output: %w", err)
		}
		return nil
	}
	if human == nil {
		return nil
	}
	return human(p.Out)
}

func (p *Printer) Infof(format string, args ...any) {
	fmt.Fprintf(p.Err, format+"\n", args...)
}
