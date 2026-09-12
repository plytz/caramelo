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
	return nil, localOnly("machine add", "it ssh's to the box it is adding, from the computer you are typing on")
}

func (LocalOnlyFleet) MachineJoin(context.Context, MachineJoinRequest, io.Writer) (*MachineJoinResult, error) {
	return nil, localOnly("machine join",
		"it is run on the machine that is joining, as root, and rewrites that machine's configuration")
}

func localOnly(what, why string) error {
	return fmt.Errorf("%s is not a command a daemon answers: %s. Run it on your own computer: %w",
		what, why, ErrNotImplemented)
}
