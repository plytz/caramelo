package sshapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/sshapi"
)

func TestRemovePeerRefusesTheSessionsOwnIdentity(t *testing.T) {
	d := &sshapi.Daemon{}
	ctx := sshapi.WithSession(context.Background(), api.Session{
		Transport: "tunnel", Identity: "commander",
	})

	err := d.RemovePeer(ctx, "commander", false)
	if err == nil {
		t.Fatal("removing this session's own peer was allowed")
	}
	for _, want := range []string{"commander", "this session arrived as", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRemovePeerAllowsEveryOtherCase(t *testing.T) {
	d := &sshapi.Daemon{}
	own := sshapi.WithSession(context.Background(), api.Session{Transport: "tunnel", Identity: "commander"})

	for _, c := range []struct {
		what  string
		ctx   context.Context
		name  string
		force bool
	}{
		{"another peer", own, "agent-7", false},
		{"its own, forced", own, "commander", true},
		{"the local socket, which has no identity", context.Background(), "commander", false},
	} {
		err := d.RemovePeer(c.ctx, c.name, c.force)
		if err != nil && strings.Contains(err.Error(), "this session arrived as") {
			t.Errorf("%s was refused by the guard: %v", c.what, err)
		}
	}
}
