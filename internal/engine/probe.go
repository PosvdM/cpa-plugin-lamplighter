package engine

import (
	"context"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
)

// probeOffset is how long before and after every reset the engine queries
// the credential once more. The query before the reset records what was
// left of the ending cycle for the history and the recovery notification;
// usage in the last probeOffset is not counted. The query after it records
// the new cycle and sends the recovery notification without waiting for
// the next poll.
const probeOffset = 30 * time.Second

// probeTolerance treats two reset times of one credential as the same
// reset; providers return slightly different values per call.
const probeTolerance = 10 * time.Second

// probeLate is how long after a reset its query after the reset is still
// run. A window whose view still shows a reset older than that, for example
// right after a restart, waits for the next poll.
const probeLate = 10 * time.Minute

// probe is one query due for a credential at the time at.
type probe struct {
	authIndex string
	at        time.Time
}

// probes returns the queries around resets that are due at now and the time
// of the next one, zero when there is none. Every window with a reset time
// is queried probeOffset before and after its reset, once each, unless a
// reading at or after that time already exists. probes does not mark
// anything; runProbes does.
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
			if w.Reset.IsZero() {
				continue
			}
			for _, slot := range []struct{ at, until time.Time }{
				{w.Reset.Add(-probeOffset), w.Reset},
				{w.Reset.Add(probeOffset), w.Reset.Add(probeLate)},
			} {
				if !now.Before(slot.until) || e.wasProbed(g.AuthIndex, slot.at) ||
					!w.ObservedAt.Before(slot.at.Add(-probeTolerance)) {
					continue
				}
				if now.Before(slot.at) {
					if next.IsZero() || slot.at.Before(next) {
						next = slot.at
					}
					continue
				}
				due = append(due, probe{authIndex: g.AuthIndex, at: slot.at})
			}
		}
	}
	return due, next
}

// wasProbed reports whether authIndex was queried for a time within
// probeTolerance of at.
func (e *Engine) wasProbed(authIndex string, at time.Time) bool {
	for _, done := range e.probed[authIndex] {
		diff := done.Sub(at)
		if diff < 0 {
			diff = -diff
		}
		if diff <= probeTolerance {
			return true
		}
	}
	return false
}

// runProbes queries every credential with a query due and records the
// query times until they no longer matter.
func (e *Engine) runProbes(ctx context.Context, cfg config.Config, now time.Time) {
	for authIndex, times := range e.probed {
		kept := times[:0]
		for _, at := range times {
			if now.Before(at.Add(probeLate)) {
				kept = append(kept, at)
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
		e.probed[p.authIndex] = append(e.probed[p.authIndex], p.at)
		if !queried[p.authIndex] {
			queried[p.authIndex] = true
			e.poll(ctx, cfg, now, p.authIndex, true)
		}
	}
}
