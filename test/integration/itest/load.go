//go:build integration

package itest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultLoadInterval = 20 * time.Millisecond
	DefaultLoadTimeout  = 10 * time.Second
)

type Load struct {
	Client   *http.Client
	URL      string
	Interval time.Duration
	Marker   func(body string) string
	Timeout  time.Duration
}

type MarkerRun struct {
	Marker      string
	Count       int
	First, Last time.Time
}

type LoadReport struct {
	Total, OK, Failed int
	Statuses          map[int]int
	Runs              []MarkerRun
	Errors            []string
	Elapsed           time.Duration
}

const maxRecordedErrors = 10

func (r LoadReport) Sequence() []string {
	out := make([]string, 0, len(r.Runs))
	for _, run := range r.Runs {
		out = append(out, run.Marker)
	}
	return out
}

func (r LoadReport) Flips() int {
	if len(r.Runs) == 0 {
		return 0
	}
	return len(r.Runs) - 1
}

func (r LoadReport) Markers() []string {
	seen := map[string]bool{}
	for _, run := range r.Runs {
		seen[run.Marker] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (r LoadReport) Rate() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Total) / r.Elapsed.Seconds()
}

func (r LoadReport) String() string {
	return fmt.Sprintf("%d requests in %s (%.0f/s): %d ok, %d failed, markers %v",
		r.Total, r.Elapsed.Round(time.Millisecond), r.Rate(), r.OK, r.Failed, r.Sequence())
}

type LoadRun struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	report LoadReport
	start  time.Time
}

func StartLoad(l Load) *LoadRun {
	if l.Interval <= 0 {
		l.Interval = Scale(DefaultLoadInterval)
	}
	if l.Timeout <= 0 {
		l.Timeout = Scale(DefaultLoadTimeout)
	}
	if l.Marker == nil {
		l.Marker = func(body string) string { return strings.TrimSpace(body) }
	}
	if l.Client == nil {
		l.Client = &http.Client{Timeout: l.Timeout}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &LoadRun{
		cancel: cancel,
		done:   make(chan struct{}),
		start:  time.Now(),
		report: LoadReport{Statuses: map[int]int{}},
	}
	go r.loop(ctx, l)
	return r
}

func (r *LoadRun) loop(ctx context.Context, l Load) {
	defer close(r.done)
	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		status, marker, err := r.one(ctx, l)
		if ctx.Err() != nil {
			return
		}
		r.record(status, marker, err)
	}
}

func (r *LoadRun) one(ctx context.Context, l Load) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, l.URL, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
	}
	return resp.StatusCode, l.Marker(string(body)), nil
}

func (r *LoadRun) record(status int, marker string, err error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.report.Total++
	r.report.Statuses[status]++
	if err != nil {
		r.report.Failed++
		if len(r.report.Errors) < maxRecordedErrors {
			r.report.Errors = append(r.report.Errors, err.Error())
		}
		return
	}
	r.report.OK++
	if n := len(r.report.Runs); n > 0 && r.report.Runs[n-1].Marker == marker {
		r.report.Runs[n-1].Count++
		r.report.Runs[n-1].Last = now
		return
	}
	r.report.Runs = append(r.report.Runs, MarkerRun{Marker: marker, Count: 1, First: now, Last: now})
}

func (r *LoadRun) Report() LoadReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *LoadRun) Stop() LoadReport {
	r.cancel()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *LoadRun) snapshotLocked() LoadReport {
	out := r.report
	out.Elapsed = time.Since(r.start)
	out.Runs = append([]MarkerRun(nil), r.report.Runs...)
	out.Errors = append([]string(nil), r.report.Errors...)
	out.Statuses = make(map[int]int, len(r.report.Statuses))
	for k, v := range r.report.Statuses {
		out.Statuses[k] = v
	}
	return out
}

func (r *LoadRun) WaitForRequests(n int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		if got := r.Report().Total; got >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the load generator made %d requests in %s, wanted %d",
				r.Report().Total, budget, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
