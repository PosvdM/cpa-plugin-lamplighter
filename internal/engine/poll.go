package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/ignite"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Cred is one monitored credential.
type Cred struct {
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Email       string `json:"email,omitempty"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	// Suffix tells accounts of the same provider apart, for example "#rk".
	Suffix string `json:"suffix,omitempty"`
}

// WindowView is a quota window with its origin.
type WindowView struct {
	quota.Window
	Source     quota.Source
	ObservedAt time.Time
}

// GroupView is the current state of one quota group.
type GroupView struct {
	Key         string
	Provider    string
	AuthIndex   string
	SourceLabel string
	Label       string
	Windows     []WindowView
}

func (g *GroupView) windows() []quota.Window {
	out := make([]quota.Window, 0, len(g.Windows))
	for _, w := range g.Windows {
		out = append(out, w.Window)
	}
	return out
}

func (g *GroupView) fiveHour() (WindowView, bool) {
	for _, w := range g.Windows {
		if quota.IsFiveHour(w.Window) {
			return w, true
		}
	}
	return WindowView{}, false
}

type usageEvent struct {
	provider  string
	authIndex string
	header    http.Header
	at        time.Time
}

type sampleMark struct {
	at        time.Time
	remaining float64
	reset     int64
}

// ObserveUsage receives a usage record from usage.handle. It only parses the
// record and queues it, so the host call returns immediately.
func (e *Engine) ObserveUsage(record pluginapi.UsageRecord) {
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	if provider != "claude" && provider != "codex" {
		return
	}
	authIndex := strings.TrimSpace(record.AuthIndex)
	if authIndex == "" || len(record.ResponseHeaders) == 0 {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = e.now()
	}
	select {
	case e.usageCh <- usageEvent{provider: provider, authIndex: authIndex, header: record.ResponseHeaders, at: at}:
	default:
		// The loop is busy; dropping one passive sample is harmless.
	}
}

func (e *Engine) drainUsage(ctx context.Context) {
	for {
		select {
		case ev := <-e.usageCh:
			e.applyUsage(ctx, ev)
		default:
			return
		}
	}
}

func (e *Engine) applyUsage(ctx context.Context, ev usageEvent) {
	now := e.now()
	group, ok := quota.ParsePassive(ev.provider, ev.header, now)
	if !ok {
		return
	}
	e.mu.Lock()
	cred := e.creds[ev.authIndex]
	e.mu.Unlock()
	if cred == nil || cred.Provider != ev.provider {
		return
	}
	cfg := e.config()
	if !cfg.Provider(cred.Provider).Monitor {
		return
	}
	e.passiveAt[ev.authIndex] = now
	e.applyGroups(ctx, cfg, cred, []quota.RawGroup{group}, quota.SourcePassive, now)
}

var emailPattern = regexp.MustCompile(`([A-Za-z0-9._%+]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`)

func credentialEmail(entry pluginapi.HostAuthFileEntry) string {
	for _, value := range []string{entry.Email, entry.Account} {
		if strings.Contains(value, "@") {
			return strings.TrimSpace(value)
		}
	}
	if match := emailPattern.FindString(entry.Name); match != "" {
		return match
	}
	return ""
}

func maskedSuffix(email string, ordinal int) string {
	local, _, _ := strings.Cut(email, "@")
	runes := []rune(local)
	switch {
	case len(runes) >= 2:
		return "#" + string(runes[len(runes)-2:])
	case len(runes) == 1:
		return "#" + local
	}
	return "#" + strconv.Itoa(ordinal)
}

func supported(provider string) bool {
	for _, name := range config.SupportedProviders {
		if name == provider {
			return true
		}
	}
	return false
}

// loadCreds reads the credential list and assigns account suffixes. A
// provider with one monitored credential needs no suffix.
func (e *Engine) loadCreds(cfg config.Config) ([]*Cred, error) {
	entries, err := e.host.AuthList()
	if err != nil {
		return nil, err
	}
	var creds []*Cred
	counts := map[string]int{}
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if !supported(provider) || entry.Disabled || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		if !cfg.Provider(provider).Monitor {
			continue
		}
		creds = append(creds, &Cred{
			ID:          entry.ID,
			AuthIndex:   entry.AuthIndex,
			Name:        entry.Name,
			Provider:    provider,
			Email:       credentialEmail(entry),
			Unavailable: entry.Unavailable,
		})
		counts[provider]++
	}
	ordinals := map[string]int{}
	for _, cred := range creds {
		ordinals[cred.Provider]++
		if counts[cred.Provider] > 1 {
			cred.Suffix = maskedSuffix(cred.Email, ordinals[cred.Provider])
		}
	}
	byIndex := make(map[string]*Cred, len(creds))
	for _, cred := range creds {
		byIndex[cred.AuthIndex] = cred
	}
	e.mu.Lock()
	e.creds = byIndex
	for key, group := range e.groups {
		cred, ok := byIndex[group.AuthIndex]
		if !ok || cred.Provider != group.Provider {
			delete(e.groups, key)
			continue
		}
		group.Label = labelFor(group.SourceLabel, cred)
	}
	e.mu.Unlock()
	return creds, nil
}

// shouldSkipActive reports whether the active query of cred can be skipped at
// slot because a passive update arrived shortly before it. Skipping stops
// after PassiveSkipMaxMinutes so that windows without passive data, such as
// Claude's Fable window, still refresh.
func (e *Engine) shouldSkipActive(cfg config.Config, cred *Cred, slot, now time.Time) bool {
	if cred.Provider == "antigravity" || cfg.PassiveSkipSeconds <= 0 {
		return false
	}
	passive, ok := e.passiveAt[cred.AuthIndex]
	if !ok || slot.Sub(passive) > time.Duration(cfg.PassiveSkipSeconds)*time.Second {
		return false
	}
	active, ok := e.activeAt[cred.AuthIndex]
	if !ok || now.Sub(active) >= time.Duration(cfg.PassiveSkipMaxMinutes)*time.Minute {
		return false
	}
	return true
}

// poll runs one round of active queries. slot is the aligned time this round
// belongs to. onlyAuth limits the round to one credential. force disables the
// passive skip rule, for the manual refresh button.
func (e *Engine) poll(ctx context.Context, cfg config.Config, slot time.Time, onlyAuth string, force bool) {
	creds, err := e.loadCreds(cfg)
	if err != nil {
		e.mu.Lock()
		e.pollErrs = map[string]string{"": "读取凭证列表失败：" + err.Error()}
		e.mu.Unlock()
		e.logf("warn", "读取凭证列表失败：%v", err)
		return
	}
	errs := map[string]string{}
	for _, cred := range creds {
		if ctx.Err() != nil {
			return
		}
		if onlyAuth != "" && cred.AuthIndex != onlyAuth {
			continue
		}
		now := e.now()
		if !force && e.shouldSkipActive(cfg, cred, slot, now) {
			continue
		}
		if err := e.refreshCred(ctx, cfg, cred); err != nil {
			errs[cred.AuthIndex] = err.Error()
			e.logf("warn", "额度刷新失败：%s：%v", labelFor(quota.ProviderTitle(cred.Provider), cred), err)
		}
	}
	e.mu.Lock()
	if onlyAuth == "" {
		e.pollErrs = errs
		e.lastPoll = e.now()
	} else {
		delete(e.pollErrs, onlyAuth)
		for key, value := range errs {
			e.pollErrs[key] = value
		}
	}
	e.mu.Unlock()
	e.dirty = true
}

// refreshCred runs the active query of one credential.
func (e *Engine) refreshCred(ctx context.Context, cfg config.Config, cred *Cred) error {
	raw, err := e.host.AuthGet(cred.AuthIndex)
	if err != nil {
		return fmt.Errorf("读取凭证文件失败：%w", err)
	}
	secret := quota.ParseSecret(raw)
	groups, err := quota.FetchActive(ctx, e.egress, cred.Provider, secret, e.now())
	if err != nil {
		return err
	}
	now := e.now()
	e.activeAt[cred.AuthIndex] = now
	e.applyGroups(ctx, cfg, cred, groups, quota.SourceActive, now)
	return nil
}

// applyGroups merges new windows into the current view, records history,
// updates the reset observation of ignition targets and runs the alerts. An
// active result replaces the group; a passive result only updates the
// windows it contains.
func (e *Engine) applyGroups(ctx context.Context, cfg config.Config, cred *Cred, raw []quota.RawGroup, source quota.Source, now time.Time) {
	var changed []*GroupView
	e.mu.Lock()
	for _, rg := range raw {
		key := cred.Provider + ":" + cred.AuthIndex + ":" + rg.Key
		group, ok := e.groups[key]
		if !ok {
			group = &GroupView{Key: key, Provider: cred.Provider, AuthIndex: cred.AuthIndex}
			e.groups[key] = group
		}
		group.SourceLabel = providerLabel(cred.Provider, rg.SourceLabel)
		group.Label = labelFor(group.SourceLabel, cred)
		if source == quota.SourceActive {
			group.Windows = group.Windows[:0]
		}
		for _, w := range rg.Windows {
			view := WindowView{Window: w, Source: source, ObservedAt: now}
			replaced := false
			for i := range group.Windows {
				if group.Windows[i].ID == w.ID {
					group.Windows[i] = view
					replaced = true
					break
				}
			}
			if !replaced {
				group.Windows = append(group.Windows, view)
			}
		}
		sort.SliceStable(group.Windows, func(i, j int) bool {
			return windowOrder(group.Windows[i].Window) < windowOrder(group.Windows[j].Window)
		})
		snapshot := *group
		snapshot.Windows = append([]WindowView(nil), group.Windows...)
		changed = append(changed, &snapshot)
	}
	e.mu.Unlock()

	for _, group := range changed {
		if w, ok := group.fiveHour(); ok {
			ignite.Observe(e.state.Target(group.Key), w.Reset, now, source == quota.SourcePassive)
		}
		e.recordSamples(group, source, now)
		if e.alerts != nil {
			e.alerts.ProcessGroup(ctx, e.state, notify.Group{Key: group.Key, Label: group.Label, Windows: group.windows()}, now)
		}
	}
	e.dirty = true
}

func windowOrder(w quota.Window) int {
	switch {
	case quota.IsFiveHour(w):
		return 0
	case w.ID == quota.WindowSevenDay:
		return 1
	case quota.IsSevenDay(w):
		return 2
	}
	return 3
}

// recordSamples writes history samples. Active samples are always written.
// Passive samples are written when the value changes, at most once per
// minute per window; a newer value inside that minute waits in pendingSamp.
func (e *Engine) recordSamples(group *GroupView, source quota.Source, now time.Time) {
	var records []store.Record
	for _, w := range group.Windows {
		if w.Source != source || !w.ObservedAt.Equal(now) {
			continue
		}
		key := group.Key + "|" + w.ID
		record := store.Record{
			Kind:      "s",
			Time:      now.Unix(),
			Group:     group.Key,
			Window:    w.ID,
			Remaining: w.Remaining,
			Source:    string(source),
		}
		if !w.Reset.IsZero() {
			record.Reset = w.Reset.Unix()
		}
		last, seen := e.lastSample[key]
		if source == quota.SourcePassive && seen {
			unchanged := last.remaining == record.Remaining && absInt64(last.reset-record.Reset) <= 10
			if unchanged {
				delete(e.pendingSamp, key)
				continue
			}
			if now.Sub(last.at) < time.Minute {
				e.pendingSamp[key] = record
				continue
			}
		}
		delete(e.pendingSamp, key)
		e.lastSample[key] = sampleMark{at: now, remaining: record.Remaining, reset: record.Reset}
		records = append(records, record)
	}
	if len(records) > 0 && e.history != nil {
		if err := e.history.Append(records...); err != nil {
			e.logf("warn", "写入历史记录失败：%v", err)
		}
	}
}

// flushSamples writes pending passive samples whose minute has passed, or
// all of them when force is set.
func (e *Engine) flushSamples(now time.Time, force bool) {
	var records []store.Record
	for key, record := range e.pendingSamp {
		last := e.lastSample[key]
		if !force && now.Sub(last.at) < time.Minute {
			continue
		}
		record.Time = now.Unix()
		e.lastSample[key] = sampleMark{at: now, remaining: record.Remaining, reset: record.Reset}
		records = append(records, record)
		delete(e.pendingSamp, key)
	}
	if len(records) > 0 && e.history != nil {
		e.history.Append(records...)
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (e *Engine) pollResetFeed(ctx context.Context, cfg config.Config) {
	records, err := notify.FetchResetRecords(ctx, &http.Client{Timeout: cfg.RequestTimeout()}, "cpa-plugin-lamplighter/"+e.version)
	if err != nil {
		e.logf("warn", "Did Codex Reset 检查失败：%v", err)
		return
	}
	var sender notify.Sender
	if e.alerts != nil {
		sender = e.alerts.Sender
	}
	sent := notify.ProcessResetRecords(ctx, e.state, records, cfg.CodexResetUpdates.NotifyCurrentPending, sender, cfg.Location(), e.now())
	if sent > 0 {
		e.addEvent("info", "codex_reset", "", fmt.Sprintf("Did Codex Reset：已发送 %d 条通知", sent))
	}
	e.dirty = true
}

var errNoTarget = errors.New("找不到该点火目标")
