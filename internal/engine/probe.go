package engine

import (
	"context"
	"strconv"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// probeLead is how long before a reset the engine queries the credential
// once more, so that the recovery notification reports what was left of
// the ending cycle. Usage in the last probeLead is not counted.
const probeLead = 2 * time.Minute

// probes returns the credentials due for a pre-reset query at now and the
// time of the next one, zero when there is none. A window is probed when its
// recovery mode is all, once per reset, unless a reading at or after the
// probe time already exists. In after_exhausted mode the ended cycle is used
// up by definition, so no probe is needed.
func (e *Engine) probes(cfg config.Config, now time.Time) (due []string, next time.Time) {
	if e.state == nil {
		return nil, time.Time{}
	}
	e.mu.Lock()
	groups := make([]GroupView, 0, len(e.groups))
	for _, g := range e.groups {
		snapshot := *g
		snapshot.Windows = append([]WindowView(nil), g.Windows...)
		groups = append(groups, snapshot)
	}
	e.mu.Unlock()

	seen := map[string]bool{}
	for key, reset := range e.probed {
		if !now.Before(reset) {
			delete(e.probed, key)
		}
	}
	for _, g := range groups {
		var states map[string]*store.WindowState
		if gs := e.state.Groups[g.Key]; gs != nil {
			states = gs.Windows
		}
		for _, w := range g.Windows {
			if w.Reset.IsZero() || !now.Before(w.Reset) {
				continue
			}
			if cfg.RecoveryNotify.For(notify.SevenDayClass(w.Window, states[w.ID])) != config.RecoveryAll {
				continue
			}
			at := w.Reset.Add(-probeLead)
			key := g.AuthIndex + "|" + strconv.FormatInt(w.Reset.Unix(), 10)
			if _, done := e.probed[key]; done || !w.ObservedAt.Before(at) {
				continue
			}
			if now.Before(at) {
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			e.probed[key] = w.Reset
			if !seen[g.AuthIndex] {
				seen[g.AuthIndex] = true
				due = append(due, g.AuthIndex)
			}
		}
	}
	return due, next
}

// runProbes queries every credential due for a pre-reset query.
func (e *Engine) runProbes(ctx context.Context, cfg config.Config, now time.Time) {
	due, _ := e.probes(cfg, now)
	for _, authIndex := range due {
		e.poll(ctx, cfg, now, authIndex, true)
	}
}
