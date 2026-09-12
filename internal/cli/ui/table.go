package ui

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type Table struct {
	head []string
	rows [][]string

	notes []string
}

func NewTable(head ...string) *Table {
	return &Table{head: head}
}

func (t *Table) Row(cells ...string) *Table {
	t.rows = append(t.rows, cells)
	return t
}

func (t *Table) Note(format string, args ...any) *Table {
	t.notes = append(t.notes, fmt.Sprintf(format, args...))
	return t
}

func (t *Table) Empty() bool { return t == nil || len(t.rows) == 0 }

func (t *Table) Rows() [][]string { return t.rows }

func (t *Table) Write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(t.head) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(t.head, "\t")); err != nil {
			return err
		}
	}
	for _, r := range t.rows {
		if _, err := fmt.Fprintln(tw, strings.Join(r, "\t")); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render table: %w", err)
	}
	return t.writeNotes(w)
}

func (t *Table) writeNotes(w io.Writer) error {
	for _, n := range t.notes {
		if _, err := fmt.Fprintln(w, n); err != nil {
			return err
		}
	}
	return nil
}

type Fields struct {
	title string
	rows  [][2]string
	notes []string
}

func NewFields(title string) *Fields { return &Fields{title: title} }

func (f *Fields) Add(label, format string, args ...any) *Fields {
	f.rows = append(f.rows, [2]string{label, fmt.Sprintf(format, args...)})
	return f
}

func (f *Fields) Note(format string, args ...any) *Fields {
	f.notes = append(f.notes, fmt.Sprintf(format, args...))
	return f
}

func (f *Fields) Write(w io.Writer) error {
	var b strings.Builder
	if f.title != "" {
		fmt.Fprintln(&b, f.title)
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range f.rows {
		fmt.Fprintf(tw, "  %s\t%s\n", r[0], r[1])
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render fields: %w", err)
	}
	for _, n := range f.notes {
		fmt.Fprintln(&b, n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
