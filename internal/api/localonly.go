package api

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var ErrNotImplemented = errors.New("not implemented in this build")

type LocalOnlyFleet struct{}

func (LocalOnlyFleet) MachineAdd(context.Context, MachineAddRequest, io.Writer) (*MachineAddResult, error) {
	return nil, localOnly("member add",
		"it ssh's to the box it is adding, from the commander, so run it on the commander and not through a daemon")
}

func (LocalOnlyFleet) MachineJoin(context.Context, MachineJoinRequest, io.Writer) (*MachineJoinResult, error) {
	return nil, localOnly("member join",
		"it is run on the machine that is joining, as root, and rewrites that machine's configuration, so run it there and not through a daemon")
}

func localOnly(what, why string) error {
	return fmt.Errorf("%s is not a command a daemon answers: %s: %w", what, why, ErrNotImplemented)
}
