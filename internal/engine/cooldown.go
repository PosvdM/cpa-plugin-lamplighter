package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

const (
	// staleCooldownMin is the shortest remaining CPA cooldown that counts as
	// stale. Shorter cooldowns end around a 5-hour reset on their own.
	staleCooldownMin = 10 * time.Minute
	// freshQuotaAge is how recent a quota must be to prove the quota is back.
	freshQuotaAge = 15 * time.Minute
)

// exhaustedWindow returns a window of the group other than the 5-hour one
// that is used up and has not reset yet. While it lasts, every request on
// the account is rejected, so ignition waits for it.
func exhaustedWindow(g GroupView, now time.Time) (WindowView, bool) {
	for _, w := range g.Windows {
		if quota.IsFiveHour(w.Window) {
			continue
		}
		if w.Remaining <= notify.ExhaustedRemaining && (w.Reset.IsZero() || w.Reset.After(now)) {
			return w, true
		}
	}
	return WindowView{}, false
}

// staleCooldown reports whether CPA keeps cred in a long cooldown although
// a fresh quota of the account shows that nothing is used up. CPA cools a
// credential down until the reset the provider reported with the 429, and
// does not lift it when the quota is reset early, for example with a reset
// card. groups are the quota groups of cred.
func staleCooldown(cred *Cred, groups []*GroupView, now time.Time) bool {
	if cred == nil || !cred.CooldownUntil.After(now.Add(staleCooldownMin)) {
		return false
	}
	for _, g := range groups {
		five, ok := g.fiveHour()
		if !ok {
			continue
		}
		fresh := now.Sub(five.ObservedAt) <= freshQuotaAge
		available := true
		for _, w := range g.Windows {
			if w.Remaining <= notify.ExhaustedRemaining {
				available = false
			}
			if now.Sub(w.ObservedAt) > freshQuotaAge {
				fresh = false
			}
		}
		if fresh && available {
			return true
		}
	}
	return false
}

// checkCooldowns sends one notification per CPA cooldown that has become
// stale, and records an event.
func (e *Engine) checkCooldowns(ctx context.Context, cfg config.Config) {
	if e.state == nil {
		return
	}
	now := e.now()
	type stale struct {
		cred  *Cred
		label string
	}
	var found []stale
	e.mu.Lock()
	for _, cred := range e.creds {
		var groups []*GroupView
		for _, g := range e.groups {
			if g.AuthIndex == cred.AuthIndex {
				groups = append(groups, g)
			}
		}
		if staleCooldown(cred, groups, now) {
			found = append(found, stale{cred: cred, label: labelFor(quota.ProviderTitle(cred.Provider), cred)})
		}
	}
	e.mu.Unlock()
	for _, s := range found {
		until := s.cred.CooldownUntil.Unix()
		if e.state.CooldownNotices[s.cred.AuthIndex] == until {
			continue
		}
		e.state.CooldownNotice(s.cred.AuthIndex, until)
		e.dirty = true
		msg := notify.CooldownMessage(e.language(), s.label, s.cred.CooldownUntil, cfg.Location())
		e.addEventDetail("warn", "cooldown_stale", "", s.label,
			fmt.Sprintf("额度已恢复，CPA 仍冷却到 %s", s.cred.CooldownUntil.In(cfg.Location()).Format("01/02 15:04")), msg.Title,
			map[string]string{"until": eventTime(s.cred.CooldownUntil)})
		if e.alerts == nil || e.alerts.Sender == nil {
			continue
		}
		if err := e.alerts.Sender.Send(ctx, msg); err != nil {
			e.notifyFailed("", msg, err)
		} else {
			e.notified("", msg)
		}
	}
}
