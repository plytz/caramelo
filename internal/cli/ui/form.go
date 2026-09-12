package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

var ErrNoTerminal = errors.New("no terminal to ask on")

var ErrNoAnswer = errors.New("nothing was typed")

type Question struct {
	In  io.Reader
	Out io.Writer
}

func (q Question) Confirm(plan string) (bool, error) {
	if q.In == nil {
		return false, ErrNoTerminal
	}
	fmt.Fprint(q.Out, plan)
	fmt.Fprint(q.Out, "Proceed? [y/N] ")
	answer, err := q.readLine()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func (q Question) ConfirmName(plan, prompt, want string) (bool, error) {
	if q.In == nil {
		return false, ErrNoTerminal
	}
	fmt.Fprint(q.Out, plan)
	fmt.Fprint(q.Out, prompt)
	answer, err := q.readLine()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(answer) == want, nil
}

func (q Question) Secret(prompt string) (string, error) {
	if q.In == nil {
		return "", ErrNoTerminal
	}
	value, err := q.readSecret(prompt)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", ErrNoAnswer
	}
	return value, nil
}

func (q Question) readSecret(prompt string) (string, error) {
	fmt.Fprint(q.Out, prompt+": ")
	f, ok := q.In.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		answer, err := q.readLine()
		return strings.TrimRight(answer, "\r\n"), err
	}
	typed, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(q.Out)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", ErrNoTerminal
		}
		return "", fmt.Errorf("read the answer: %w", err)
	}
	return strings.TrimRight(string(typed), "\r\n"), nil
}

func (q Question) readLine() (string, error) {
	answer, err := bufio.NewReader(q.In).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the answer: %w", err)
	}
	if strings.TrimSpace(answer) == "" && errors.Is(err, io.EOF) {
		return "", ErrNoTerminal
	}
	return answer, nil
}
