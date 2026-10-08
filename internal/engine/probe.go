package engine

import (
	"context"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
)

// probeLead is how long before every reset the engine queries the
// credential once more, so that the history and the recovery notification
// show what was left of the ending cycle. Usage in the last probeLead is not
// counted.
const probeLead = 30 * time.Second

// probeTolerance treats two reset times of one credential as the same
// reset; providers return slightly different values per call.
const probeTolerance = 10 * time.Second

type probe struct {
	authIndex string
	reset     time.Time
}

// probes returns the pre-reset queries due at now and the time of the next
// one, zero when there is none. Every window with a reset time is probed
// once per reset, unless a reading at or after the probe time already
// exists. probes does not mark anything; runProbes does.
func (e *Engine) probes(now time.Time) (due []probe, next time.Time) {
	e.mu.Lock()
	groups := make([]GroupView, 0, len(e.groups))
	for _, g := range e.groups {
		snapshot := *g
		snapshot.Windows = append([]WindowView(nil), g.Windows...)
		groups = append(groups, snapshot)
	}
	e.mu.Unlock()

	for _, g := range groups {
		for _, w := range g.Windows {
			if w.Reset.IsZero() || !now.Before(w.Reset) {
				continue
			}
			at := w.Reset.Add(-probeLead)
			if e.wasProbed(g.AuthIndex, w.Reset) || !w.ObservedAt.Before(at.Add(-probeTolerance)) {
				continue
			}
			if now.Before(at) {
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			due = append(due, probe{authIndex: g.AuthIndex, reset: w.Reset})
		}
	}
	return due, next
}

// wasProbed reports whether authIndex was queried for a reset within
// probeTolerance of reset.
func (e *Engine) wasProbed(authIndex string, reset time.Time) bool {
	for _, done := range e.probed[authIndex] {
		diff := done.Sub(reset)
		if diff < 0 {
			diff = -diff
		}
		if diff <= probeTolerance {
			return true
		}
	}
	return false
}

// runProbes queries every credential due for a pre-reset query and records
// the resets it was queried for until they pass.
func (e *Engine) runProbes(ctx context.Context, cfg config.Config, now time.Time) {
	for authIndex, resets := range e.probed {
		kept := resets[:0]
		for _, reset := range resets {
			if now.Before(reset.Add(probeTolerance)) {
				kept = append(kept, reset)
			}
		}
		if len(kept) == 0 {
			delete(e.probed, authIndex)
		} else {
			e.probed[authIndex] = kept
		}
	}
	due, _ := e.probes(now)
	queried := map[string]bool{}
	for _, p := range due {
		e.probed[p.authIndex] = append(e.probed[p.authIndex], p.reset)
		if !queried[p.authIndex] {
			queried[p.authIndex] = true
			e.poll(ctx, cfg, now, p.authIndex, true)
		}
	}
}
