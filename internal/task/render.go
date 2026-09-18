package task

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/progress"
)

type Plan struct {
	Task   string
	Desc   string
	DryRun bool
	On     []string
	Vars   []Var
	Items  []PlanItem
}

type PlanItem struct {
	Name  string
	Desc  string
	Block bool
	Items []PlanItem
}

func planOf(f *File) Plan {
	return Plan{Task: f.Name, Desc: f.Desc, Items: planItems(f.Items)}
}

func planItems(items []Entry) []PlanItem {
	out := make([]PlanItem, 0, len(items))
	for _, e := range items {
		if e.Block != nil {
			out = append(out, PlanItem{Name: blockName, Desc: e.Block.Desc, Block: true, Items: planItems(e.Block.Items)})
			continue
		}
		out = append(out, PlanItem{Name: e.Item.Name, Desc: e.Item.Desc})
	}
	return out
}

const (
	minNameWidth  = 17
	minStatusWide = 10
	detailGap     = 3
	kindWidth     = 7
)

const (
	ansiReset    = "\x1b[0m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiBlue     = "\x1b[34m"
	ansiRed      = "\x1b[31m"
	ansiBoldBlue = "\x1b[1;34m"
)

type Renderer struct {
	W io.Writer

	Colour bool

	Debug bool

	Fleet bool

	plan      Plan
	nameWidth int
	statusW   int
	nodes     []*node
	byName    map[string]*node
	printed   map[string]bool
	shown     map[*node]bool
	err       error
}

type node struct {
	name   string
	desc   string
	depth  int
	blocks []*node
}

func (r *Renderer) Start(p Plan) error {
	r.plan = p
	r.printed = map[string]bool{}
	r.shown = map[*node]bool{}
	r.byName = map[string]*node{}
	r.nodes = nil
	r.collect(p.Items, 0, nil)
	r.nameWidth = minNameWidth
	for _, n := range r.nodes {
		if w := 2*n.depth + len(n.name) + 2; w > r.nameWidth {
			r.nameWidth = w
		}
	}
	r.statusW = minStatusWide
	if p.DryRun {
		r.statusW = len(StatusWouldChange) + detailGap
	}

	head := p.Task
	if p.Desc != "" {
		head += "  " + p.Desc
	}
	if p.DryRun {
		head += "  (dry run)"
	}
	if len(p.On) > 0 {
		head += "  on " + strings.Join(p.On, " ")
	}
	r.line(head)
	if r.Debug && len(p.Vars) > 0 {
		var pairs []string
		for _, v := range p.Vars {
			pairs = append(pairs, v.Name+"="+v.Value)
		}
		r.line("  vars  " + strings.Join(pairs, "  "))
	}
	r.line("")
	return r.err
}

func (r *Renderer) collect(items []PlanItem, depth int, blocks []*node) {
	for i := range items {
		it := items[i]
		n := &node{name: it.Name, desc: it.Desc, depth: depth, blocks: blocks}
		if it.Block {
			r.nodes = append(r.nodes, n)
			r.collect(it.Items, depth+1, append(append([]*node{}, blocks...), n))
			continue
		}
		r.nodes = append(r.nodes, n)
		r.byName[it.Name] = n
	}
}

func (r *Renderer) Event(e progress.Event) error {
	if e.Action != progress.ActionTask || e.Step == "" || e.Status == progress.StatusStarted {
		return r.err
	}
	res, ok := ResultOf(e)
	if !ok {
		res = Result{Name: e.Step, Status: e.Status, Detail: e.Detail}
	}
	r.item(e.Machine, res)
	return r.err
}

func (r *Renderer) Result(res Result) error {
	r.item("", res)
	return r.err
}

func (r *Renderer) item(machine string, res Result) {
	n := r.byName[res.Name]
	depth := 0
	if n != nil {
		depth = n.depth
		for _, b := range n.blocks {
			if r.shown[b] {
				continue
			}
			r.shown[b] = true
			r.line(strings.Repeat(" ", 2+2*b.depth) + r.paint(ansiBoldBlue, b.name) + "  " + b.desc)
		}
	}
	r.printed[key(machine, res.Name)] = true
	if r.Debug {
		r.trace(machine, depth, res)
	}
	prefix := ""
	if r.Fleet && machine != "" {
		prefix = "[" + machine + "] "
	}
	name := strings.Repeat(" ", 2+2*depth) + prefix + pad(res.Name, r.nameWidth-2*depth)
	status := r.paint(colourOf(res.Status), res.Status) + strings.Repeat(" ", max(0, r.statusW-len(res.Status)))
	detail := firstLine(res.Detail)
	if res.Status == StatusFailed && detail == "" {
		detail = firstLine(res.Error)
	}
	r.line(strings.TrimRight(name+status+detail, " "))
	if res.Status == StatusFailed && !r.Debug {
		if out := failureOutput(res); out != "" {
			r.line(strings.Repeat(" ", 2+r.nameWidth) + out)
		}
	}
}

func failureOutput(res Result) string {
	for i := len(res.Commands) - 1; i >= 0; i-- {
		if s := firstLine(res.Commands[i].Stderr); s != "" {
			return s
		}
	}
	for i := len(res.Commands) - 1; i >= 0; i-- {
		if s := firstLine(res.Commands[i].Stdout); s != "" {
			return s
		}
	}
	return ""
}

func (r *Renderer) trace(machine string, depth int, res Result) {
	col := strings.Repeat(" ", 2+r.nameWidth-2)
	prefix := ""
	if r.Fleet && machine != "" {
		prefix = "[" + machine + "] "
	}
	for _, c := range res.Commands {
		head := strings.Repeat(" ", 2+2*depth) + prefix + pad(res.Name, r.nameWidth-2-2*depth)
		r.line(strings.TrimRight(head+pad(c.Kind, kindWidth)+c.Script, " "))
		r.line(col + "exit " + strconv.Itoa(c.Exit) + "  " + shortDuration(c.DurationMS))
		for _, out := range []struct{ what, body string }{{"stdout", c.Stdout}, {"stderr", c.Stderr}} {
			body := strings.TrimRight(out.body, "\n")
			if body == "" {
				continue
			}
			r.line(col + out.what + ":")
			for _, l := range strings.Split(body, "\n") {
				r.line(strings.TrimRight(col+l, " "))
			}
		}
	}
}

func (r *Renderer) Finish(reports ...Report) error {
	if len(reports) > 0 && !r.Fleet {
		r.rest(reports[0].Results)
	}
	r.line("")
	for _, rep := range reports {
		label := rep.Task
		if r.Fleet && rep.Machine != "" {
			label = rep.Machine
		}
		r.line(label + "  " + r.counts(rep) + "   " + seconds(rep.DurationMS))
	}
	for _, rep := range reports {
		if r.Debug {
			continue
		}
		if failed, ok := rep.FirstFailure(); ok {
			label := rep.Task
			if r.Fleet && rep.Machine != "" {
				label = rep.Machine
			}
			r.line(fmt.Sprintf("%s failed at %s; run again with --debug for the full output", label, failed.Name))
		}
	}
	if r.Debug {
		for _, rep := range reports {
			if rep.Path != "" {
				r.line("report  " + rep.Path)
			}
		}
	}
	return r.err
}

func (r *Renderer) rest(results []Result) {
	for _, res := range results {
		if res.Block {
			r.rest(res.Results)
			continue
		}
		if r.printed[key("", res.Name)] {
			continue
		}
		r.item("", res)
	}
}

func (r *Renderer) counts(rep Report) string {
	changed := "changed=" + strconv.Itoa(rep.Changed)
	word := StatusChanged
	if rep.DryRun {
		changed, word = "would-change="+strconv.Itoa(rep.WouldChange), StatusWouldChange
	}
	return strings.Join([]string{
		r.paint(colourOf(StatusOK), "ok="+strconv.Itoa(rep.OK)),
		r.paint(colourOf(word), changed),
		r.paint(colourOf(StatusSkipped), "skipped="+strconv.Itoa(rep.Skipped)),
		r.paint(colourOf(StatusFailed), "failed="+strconv.Itoa(rep.Failed)),
	}, "  ")
}

func (r *Renderer) paint(colour, s string) string {
	if !r.Colour || colour == "" {
		return s
	}
	return colour + s + ansiReset
}

func (r *Renderer) line(s string) {
	if r.err != nil {
		return
	}
	if _, err := fmt.Fprintln(r.W, s); err != nil {
		r.err = fmt.Errorf("write the run of %s: %w", r.plan.Task, err)
	}
}

func colourOf(status string) string {
	switch status {
	case StatusOK:
		return ansiGreen
	case StatusChanged, StatusWouldChange:
		return ansiYellow
	case StatusSkipped:
		return ansiBlue
	case StatusFailed:
		return ansiRed
	}
	return ""
}

func key(machine, name string) string { return machine + "\x00" + name }

func pad(s string, width int) string {
	if len(s) >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-len(s))
}

func shortDuration(ms int64) string {
	if ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return seconds(ms)
}

func seconds(ms int64) string {
	s := math.Round(float64(ms)/10) / 100
	return strconv.FormatFloat(s, 'f', -1, 64) + "s"
}
