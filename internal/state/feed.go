package state

import (
	"context"
	"sync"

	"github.com/plytz/caramelo/internal/progress"
)

func (e EnvEvent) Progress() progress.Event {
	return progress.Event{
		Seq:      e.ID,
		App:      e.App,
		Env:      e.Env,
		Service:  e.Service,
		Replica:  e.Replica,
		Action:   e.Action,
		Step:     e.Step,
		Status:   e.Status,
		Detail:   e.Detail,
		Identity: e.Identity,
		Machine:  e.Machine,
		At:       e.At,
		JSON:     rawJSON(e.JSON),
	}
}

func EventFromProgress(envID int64, e progress.Event) EnvEvent {
	return EnvEvent{
		ID:       e.Seq,
		EnvID:    envID,
		Action:   e.Action,
		Status:   e.Status,
		Detail:   e.Detail,
		Service:  e.Service,
		Replica:  e.Replica,
		Step:     e.Step,
		Identity: e.Identity,
		Machine:  e.Machine,
		JSON:     string(e.JSON),
		At:       e.At,
		App:      e.App,
		Env:      e.Env,
	}
}

func rawJSON(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

type notifiers struct {
	mu sync.RWMutex
	n  progress.Notifier
}

func (s *store) SetNotifier(n progress.Notifier) {
	s.notifiers.mu.Lock()
	defer s.notifiers.mu.Unlock()
	s.notifiers.n = n
}

func (s *store) notifier() progress.Notifier {
	s.notifiers.mu.RLock()
	defer s.notifiers.mu.RUnlock()
	return s.notifiers.n
}

func (s *store) publish(ctx context.Context, e EnvEvent) {
	n := s.notifier()
	if n == nil {
		return
	}
	if (e.App == "" || e.Env == "") && e.EnvID != 0 {
		if rec, err := s.envByID(ctx, e.EnvID); err == nil {
			e.App, e.Env = rec.App, rec.Name
		}
	}
	n.Notify(e.Progress())
}

func (s *store) envByID(ctx context.Context, id int64) (*EnvRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+envColumns+` FROM envs WHERE id = ?`, id)
	rec, err := scanEnv(row)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}
