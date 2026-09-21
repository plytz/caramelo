package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/place"
)

type when struct {
	text string
	op   string
	ok   func(c place.Context) bool
}

const (
	opAnd = "and"
	opOr  = "or"
	opNot = "not"
)

var (
	always = atom("any machine", func(place.Context) bool { return true })

	fresh = atom("a box with no config at all", place.Context.IsFresh)

	onCommander = atom("a commander", place.Context.IsCommander)

	onHub = atom("a hub", place.Context.IsHub)

	onMember = atom("a member", place.Context.IsMember)

	onServer = onHub.or(onMember)

	daemonUp = atom("a machine whose caramelod is answering", func(c place.Context) bool {
		return c.Server != nil && c.Server.Services.Daemon.Present
	})

	inCheckout = atom("inside an app checkout", func(c place.Context) bool { return c.Work.App != "" })

	inWorktree = atom("inside an environment worktree", func(c place.Context) bool { return c.Work.Env != "" })

	hasFleet = atom("a machine with a fleet to talk to", func(c place.Context) bool {
		return c.TalksTo.Kind != place.TalksNothing
	})

	asRoot = atom("as root", func(c place.Context) bool { return c.Root })
)

func atom(text string, ok func(c place.Context) bool) when {
	return when{text: text, ok: ok}
}

func (w when) and(o when) when {
	return when{
		text: w.clause(opAnd) + ", " + o.clause(opAnd),
		op:   opAnd,
		ok:   func(c place.Context) bool { return w.ok(c) && o.ok(c) },
	}
}

func (w when) or(o when) when {
	return when{
		text: w.clause(opOr) + " or " + o.clause(opOr),
		op:   opOr,
		ok:   func(c place.Context) bool { return w.ok(c) || o.ok(c) },
	}
}

func not(w when) when {
	return when{
		text: "anything but " + w.clause(opNot),
		op:   opNot,
		ok:   func(c place.Context) bool { return !w.ok(c) },
	}
}

func (w when) clause(under string) string {
	if w.op == "" || w.op == under {
		return w.text
	}
	return "(" + w.text + ")"
}

const whenAnnotation = "when"

var whens sync.Map

func available(cmd *cobra.Command, w when) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[whenAnnotation] = w.text
	whens.Store(w.text, w)
	return cmd
}

func whenOf(cmd *cobra.Command) (when, bool) {
	text := cmd.Annotations[whenAnnotation]
	if text == "" {
		return when{}, false
	}
	w, ok := whens.Load(text)
	if !ok {
		return when{}, false
	}
	return w.(when), true
}

const experimentalEnv = "CARAMELO_EXPERIMENTAL"

func experimentalIsOn() bool { return os.Getenv(experimentalEnv) != "" }

type approval struct {
	where string
	names []string
}

func (ap approval) covers(name string) bool { return slices.Contains(ap.names, name) }

func approvalFor(c place.Context) (approval, bool) {
	if cannotSayWhereThisIs(c) {
		return approval{}, false
	}
	switch {
	case c.IsServer():
		return approval{where: "a hub or a member", names: approvedOnServer}, true
	case c.IsFresh(), c.IsCommander():
		return approval{where: "a commander", names: approvedOnCommander}, true
	}
	return approval{}, false
}

func isMachinery(cmd *cobra.Command) bool {
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		if c.Hidden {
			return true
		}
	}
	return false
}

func dormantHere(cmd *cobra.Command, c place.Context) (approval, bool) {
	if experimentalIsOn() || isMachinery(cmd) {
		return approval{}, false
	}
	if _, ok := whenOf(cmd); !ok {
		return approval{}, false
	}
	ap, ok := approvalFor(c)
	if !ok || ap.covers(leafName(cmd)) {
		return approval{}, false
	}
	return ap, true
}

func shownHere(cmd *cobra.Command, c place.Context) bool {
	w, ok := whenOf(cmd)
	if !ok {
		return true
	}
	if !w.ok(c) {
		return false
	}
	_, dormant := dormantHere(cmd, c)
	return !dormant
}

type notHereError struct{ msg string }

func (e *notHereError) Error() string { return e.msg }

func (a *app) beforeRun(cmd *cobra.Command) error {
	if err := a.setProgress(); err != nil {
		return err
	}
	a.readConfigDirFlag(cmd)
	return a.refuseWhereItDoesNotBelong(cmd)
}

func (a *app) refuseWhereItDoesNotBelong(cmd *cobra.Command) error {
	if a.service != nil || cmd.HasSubCommands() {
		return nil
	}
	w, ok := whenOf(cmd)
	if !ok {
		return nil
	}
	c, err := a.place(cmd.Context())
	if err != nil || cannotSayWhereThisIs(c) {
		return nil
	}
	if w.ok(c) {
		return refuseWhatIsNotApproved(cmd, c)
	}
	return &notHereError{fmt.Sprintf("%s runs on %s, and %s; %s",
		leafName(cmd), w.text, whatThisIs(c), whatToDo(c, w))}
}

func refuseWhatIsNotApproved(cmd *cobra.Command, c place.Context) error {
	ap, dormant := dormantHere(cmd, c)
	if !dormant {
		return nil
	}
	return &notHereError{fmt.Sprintf("%s is not approved on %s yet; set %s=1 to run a dormant command",
		leafName(cmd), ap.where, experimentalEnv)}
}

func cannotSayWhereThisIs(c place.Context) bool {
	return c.Role == place.RoleUnknown
}

func leafName(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

func whatThisIs(c place.Context) string {
	switch {
	case c.IsFresh():
		return "this box has no config at all"
	case c.IsCommander():
		return "this machine is a commander"
	case c.IsHub():
		return "this machine is a hub"
	case c.IsMember():
		return "this machine is a member"
	}
	return "this machine's config cannot be read"
}

func whatToDo(c place.Context, w when) string {
	if c.IsFresh() && holdsOn(w, place.CanonicalCommander) {
		return "run 'caramelo commander init' to name this machine"
	}
	if !c.IsServer() && holdsOn(w, place.CanonicalHub) {
		return "run 'sudo caramelo hub setup' to make this machine a hub"
	}
	return "'caramelo context' says where you are"
}

func holdsOn(w when, canonical string) bool {
	c, ok := place.Canonical(canonical)
	return ok && w.ok(c)
}
