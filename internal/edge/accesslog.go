package edge

import (
	"encoding/json"
	"io"
	"sync"
)

type accessWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newAccessWriter(w io.Writer) *accessWriter { return &accessWriter{w: w} }

func (a *accessWriter) log(e AccessLog) {
	if a == nil || a.w == nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	line := make([]byte, 0, len(AccessLogPrefix)+len(b)+2)
	line = append(line, AccessLogPrefix...)
	line = append(line, ' ')
	line = append(line, b...)
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(line)
}
