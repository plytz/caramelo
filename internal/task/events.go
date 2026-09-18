package task

import (
	"encoding/json"

	"github.com/plytz/caramelo/internal/progress"
)

func ResultOf(e progress.Event) (Result, bool) {
	if e.Action != progress.ActionTask || len(e.JSON) == 0 {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(e.JSON, &res); err != nil {
		return Result{}, false
	}
	if res.Name == "" || res.Status == "" {
		return Result{}, false
	}
	return res, true
}
