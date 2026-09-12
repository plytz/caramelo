package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func (a *app) question(ctx context.Context) (ui.Question, bool) {
	in := stdinFrom(ctx)
	if !isTerminal(in) {
		return ui.Question{}, false
	}
	return ui.Question{In: in, Out: a.stderr}, true
}

func ask(needs error, fn func() (bool, error), ok bool) (bool, error) {
	if !ok {
		return false, needs
	}
	answer, err := fn()
	if errors.Is(err, ui.ErrNoTerminal) {

		return false, needs
	}
	return answer, err
}

func (a *app) confirm(ctx context.Context, plan string) (bool, error) {
	return a.confirmWith(ctx, errNeedsYes, plan)
}

func (a *app) confirmWith(ctx context.Context, needs error, plan string) (bool, error) {
	q, there := a.question(ctx)
	return ask(needs, func() (bool, error) { return q.Confirm(plan) }, there)
}

func (a *app) confirmName(ctx context.Context, needs error, plan, prompt, want string) (bool, error) {
	q, there := a.question(ctx)
	return ask(needs, func() (bool, error) { return q.ConfirmName(plan, prompt, want) }, there)
}

func (a *app) secretValue(ctx context.Context, name string) (string, error) {
	q, there := a.question(ctx)
	if !there {
		return "", errNeedsSecretValue(name)
	}
	value, err := q.Secret(fmt.Sprintf("Value for %s", name))
	switch {
	case errors.Is(err, ui.ErrNoTerminal):
		return "", errNeedsSecretValue(name)
	case errors.Is(err, ui.ErrNoAnswer):
		return "", errEmptySecretValue(name)
	}
	return value, err
}

func errEmptySecretValue(name string) error {
	return &usageError{fmt.Errorf(
		"nothing was typed for %s: type the value, or pass %s= to set it to the empty string",
		name, name)}
}

func errNeedsSecretValue(name string) error {
	return &usageError{fmt.Errorf(
		"no value for %s: pass %s=VALUE, or --from-file FILE, or run this where there is a terminal to type it on",
		name, name)}
}
