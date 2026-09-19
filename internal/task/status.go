package task

import "github.com/plytz/caramelo/internal/progress"

const (
	StatusOK = progress.StatusOK

	StatusChanged = progress.StatusChanged

	StatusWouldChange = progress.StatusWouldChange

	StatusSkipped = progress.StatusSkipped

	StatusFailed = progress.StatusFailed

	StatusNotRun = "not run"
)

var Statuses = []string{StatusOK, StatusChanged, StatusWouldChange, StatusSkipped, StatusFailed, StatusNotRun}

const (
	KindWhen = "when"

	KindCheck = "check"

	KindCmd = "cmd"
)
