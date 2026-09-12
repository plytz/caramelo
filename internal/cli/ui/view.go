package ui

import (
	"fmt"
	"io"
	"strings"
)

type View struct {
	blocks []block
}

type block interface {
	write(w io.Writer) error
}

func NewView() *View { return &View{} }

func (v *View) Text(format string, args ...any) *View {
	v.blocks = append(v.blocks, textBlock(fmt.Sprintf(format, args...)))
	return v
}

func (v *View) Raw(s string) *View {
	v.blocks = append(v.blocks, rawBlock(s))
	return v
}

func (v *View) Blank() *View {
	v.blocks = append(v.blocks, textBlock(""))
	return v
}

func (v *View) Table(t *Table) *View {
	if !t.Empty() {
		v.blocks = append(v.blocks, tableBlock{t})
	}
	return v
}

func (v *View) Section(t *Table) *View {
	if t.Empty() {
		return v
	}
	return v.Blank().Table(t)
}

func (v *View) Fields(f *Fields) *View {
	if f != nil {
		v.blocks = append(v.blocks, fieldsBlock{f})
	}
	return v
}

func (v *View) Write(w io.Writer) error {
	if v == nil {
		return nil
	}
	for _, b := range v.blocks {
		if err := b.write(w); err != nil {
			return err
		}
	}
	return nil
}

func (v *View) String() string {
	var b strings.Builder
	_ = v.Write(&b)
	return b.String()
}

type textBlock string

func (t textBlock) write(w io.Writer) error {
	_, err := fmt.Fprintln(w, string(t))
	return err
}

type rawBlock string

func (r rawBlock) write(w io.Writer) error {
	_, err := io.WriteString(w, string(r))
	return err
}

type tableBlock struct{ t *Table }

func (b tableBlock) write(w io.Writer) error { return b.t.Write(w) }

type fieldsBlock struct{ f *Fields }

func (b fieldsBlock) write(w io.Writer) error { return b.f.Write(w) }
