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
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/providers"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Cred is one monitored credential.
type Cred struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	// HostProvider is the provider name CPA reports, which can be an alias
	// of Provider, such as "kimi-ai" for "kimi".
	HostProvider string `json:"host_provider,omitempty"`
	Email        string `json:"email,omitempty"`
	Disabled     bool   `json:"disabled"`
	Unavailable  bool   `json:"unavailable"`
	// CooldownUntil is when CPA lets the credential serve requests again,
	// or zero when it is not cooling down.
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	// Note is the credential note set in CPA. Its first line names the
	// account.
	Note string `json:"note,omitempty"`
	// Suffix tells accounts apart: "#" and the account name, such as
	// "#主力号", or the masked email of an unnamed account, such as "#rk".
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
	// active is set once an active query has described the group.
	active bool
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
	p := providers.Lookup(record.Provider)
	if p == nil || p.Headers == nil {
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
	case e.usageCh <- usageEvent{provider: p.ID, authIndex: authIndex, header: record.ResponseHeaders, at: at}:
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
	// Relative reset times count from the request, not from when the usage
	// record is processed; a long request delivers its record late.
	groups := providers.Lookup(ev.provider).Headers(ev.header, ev.at)
	if len(groups) == 0 {
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
	e.notePassiveFable(cred, groups)
	e.applyGroups(ctx, cfg, cred, groups, quota.SourcePassive, now, true)
}

// notePassiveFable logs the first passive Fable window seen for a credential
// in this process. Regular Claude requests do not carry the 7d_oi headers;
// the log line shows whether a Fable request does.
func (e *Engine) notePassiveFable(cred *Cred, groups []quota.RawGroup) {
	for _, g := range groups {
		if g.Key != providers.FableGroupKey || e.fableSeen[cred.AuthIndex] {
			continue
		}
		if e.fableSeen == nil {
			e.fableSeen = map[string]bool{}
		}
		e.fableSeen[cred.AuthIndex] = true
		w := g.Windows[0]
		e.logf("info", "首次从响应头读到 Fable 额度：%s，剩余 %.1f%%，重置 %s",
			labelFor(providers.Title(cred.Provider), cred), w.Remaining, w.Reset.Format(time.RFC3339))
	}
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

// maxNameRunes is the longest account name; a longer one is cut and ends
// with "…".
const maxNameRunes = 20

// accountName returns the account name in a CPA credential note: its first
// line, cut to maxNameRunes characters.
func accountName(note string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(note), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > maxNameRunes {
		return string(runes[:maxNameRunes]) + "…"
	}
	return string(runes)
}

// loadCreds reads the credential list and assigns account suffixes. A named
// account gets "#" and its name. An unnamed account gets the masked email,
// unless it is the only monitored credential of its provider. A named account
// whose suffix equals that of another account of its provider, such as two
// accounts with the same name, also gets the masked email.
func (e *Engine) loadCreds(cfg config.Config) ([]*Cred, error) {
	entries, err := e.host.AuthList()
	if err != nil {
		return nil, err
	}
	var creds []*Cred
	counts := map[string]int{}
	for _, entry := range entries {
		hostProvider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if hostProvider == "" {
			hostProvider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		p := providers.Lookup(hostProvider)
		if p == nil || entry.Disabled || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		provider := p.ID
		if !cfg.Provider(provider).Monitor {
			continue
		}
		creds = append(creds, &Cred{
			ID:           entry.ID,
			AuthIndex:    entry.AuthIndex,
			Name:         entry.Name,
			Provider:     provider,
			HostProvider: hostProvider,
			Email:        credentialEmail(entry),
			Note:         entry.Note,
			Unavailable:  entry.Unavailable,
		})
		if entry.Unavailable {
			creds[len(creds)-1].CooldownUntil = entry.NextRetryAfter
		}
		counts[provider]++
	}
	ordinals := map[string]int{}
	masked := make([]string, len(creds))
	taken := map[[2]string]int{}
	for i, cred := range creds {
		ordinals[cred.Provider]++
		if counts[cred.Provider] > 1 {
			masked[i] = maskedSuffix(cred.Email, ordinals[cred.Provider])
		}
		cred.Suffix = masked[i]
		if name := accountName(cred.Note); name != "" {
			cred.Suffix = "#" + name
		}
		taken[[2]string{cred.Provider, cred.Suffix}]++
	}
	for i, cred := range creds {
		if accountName(cred.Note) != "" && taken[[2]string{cred.Provider, cred.Suffix}] > 1 {
			cred.Suffix += masked[i]
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
// Claude's Fable window, still refresh. Providers without quota in their
// response headers are never skipped.
func (e *Engine) shouldSkipActive(cfg config.Config, cred *Cred, slot, now time.Time) bool {
	if p := providers.Lookup(cred.Provider); p == nil || p.Headers == nil || cfg.PassiveSkipSeconds <= 0 {
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
			e.logf("warn", "额度刷新失败：%s：%v", labelFor(providers.Title(cred.Provider), cred), err)
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
	e.checkCooldowns(ctx, cfg)
}

// refreshCred runs the active query of one credential.
func (e *Engine) refreshCred(ctx context.Context, cfg config.Config, cred *Cred) error {
	raw, err := e.host.AuthGet(cred.AuthIndex)
	if err != nil {
		return fmt.Errorf("读取凭证文件失败：%w", err)
	}
	p := providers.Lookup(cred.Provider)
	if p == nil {
		return fmt.Errorf("不支持的额度 provider: %s", cred.Provider)
	}
	credential := providers.ParseCredential(raw)
	credential.Provider, credential.ID, credential.Name = cred.HostProvider, cred.ID, cred.Name
	groups, err := providers.Fetch(ctx, p, e.egress, credential, e.now())
	if err != nil {
		return err
	}
	now := e.now()
	e.activeAt[cred.AuthIndex] = now
	// The plan comes first, so that a changed plan resets the notification
	// baselines before the new windows are compared with them.
	confirmed := e.notePlan(ctx, p, cred, credential, groups, now)
	e.applyGroups(ctx, cfg, cred, groups, quota.SourceActive, now, confirmed)
	return nil
}

const (
	// planCheckPeriod is how often the plan of an account is requested on
	// its own, for providers whose usage response does not state it.
	planCheckPeriod = 24 * time.Hour
	// planRecheckPeriod is how long the plan is requested with every active
	// query after the windows of the account changed.
	planRecheckPeriod = time.Hour
	// planRetryPeriod is how long the next plan request waits after a failed
	// recheck, also once the recheck has ended. It goes out with the first
	// active query after that.
	planRetryPeriod = 15 * time.Minute
	// planNoticeAge is how long an undelivered plan change notification is
	// retried.
	planNoticeAge = time.Hour
)

// windowSet lists the quota groups and windows of an active result, sorted,
// to tell when an account starts or stops reporting a window.
func windowSet(groups []quota.RawGroup) string {
	var ids []string
	for _, g := range groups {
		for _, w := range g.Windows {
			ids = append(ids, g.Key+"|"+w.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\n")
}

// notePlan records the plan of cred after a successful quota query: from the
// usage response when it states one, otherwise from the provider's own
// request. That request is sent once per planCheckPeriod, and with every
// active query for planRecheckPeriod after the account reports other
// windows than before: an expired Google AI Pro plan, for one, removes the
// 5-hour windows of Antigravity. Rechecking covers a failed request and a
// plan that changes a little later than the windows. In the latter case the
// quota alerts of the queries before the change compare with the old
// baselines, so a false recovery can still be sent once. A failed request
// keeps the plan read before and does not affect the quota; after a failed
// recheck, the next request follows with the first active query
// planRetryPeriod later even when the recheck has ended. notePlan reports false when a recheck failed for
// an account with a known plan: the quota alerts of the account then wait
// for the next query, as the windows may belong to a plan not read yet.
func (e *Engine) notePlan(ctx context.Context, p *providers.Provider, cred *Cred, credential providers.Credential, groups []quota.RawGroup, now time.Time) bool {
	if p.Plan == nil {
		return true
	}
	state := e.state.PlanFor(cred.AuthIndex)
	if windows := windowSet(groups); windows != state.Windows {
		if state.Windows != "" {
			state.RecheckUntil = now.Add(planRecheckPeriod).Unix()
		}
		state.Windows = windows
		e.dirty = true
	}
	raw := ""
	for _, g := range groups {
		if g.Plan != "" {
			raw = g.Plan
		}
	}
	rechecking := now.Unix() < state.RecheckUntil
	confirmed := true
	if raw == "" && p.Profile != nil && (rechecking || now.Sub(time.Unix(state.Checked, 0)) >= planCheckPeriod) {
		state.Checked = now.Unix()
		e.dirty = true
		plan, err := p.Profile(ctx, e.egress, credential, now)
		if err != nil {
			e.logf("debug", "读取套餐失败：%s：%v", labelFor(providers.Title(cred.Provider), cred), err)
			if rechecking {
				// The daily request follows soon when the recheck ends
				// without a successful read.
				state.Checked = now.Add(planRetryPeriod - planCheckPeriod).Unix()
			}
			// Without a plan read before, a later read only records the plan
			// and leaves the baselines, so waiting would gain nothing.
			confirmed = !rechecking || state.Plan == ""
		}
		raw = plan
	}
	if raw != "" && raw != state.Plan {
		old := state.Plan
		state.Plan = raw
		e.dirty = true
		e.mu.Lock()
		e.plans[cred.AuthIndex] = raw
		e.mu.Unlock()
		from, to := providers.PlanOf(cred.Provider, old).Name, providers.PlanOf(cred.Provider, raw).Name
		if old != "" && from != to {
			state.RecheckUntil = 0
			e.planChanged(cred, state, from, to, now)
		}
	}
	e.sendPlanNotice(ctx, cred, state, now)
	return confirmed
}

// planChanged records a changed plan for its notification and marks the
// notification baselines of the account for a new start. The quota of the
// new plan is unrelated to the old one: a fresh 7-day quota on an expired
// plan is no recovery, and the quota that returns with a renewal is no drop.
// Like a window seen for the first time, each window then only records its
// baseline.
func (e *Engine) planChanged(cred *Cred, state *store.PlanState, from, to string, now time.Time) {
	prefix := cred.Provider + ":" + cred.AuthIndex + ":"
	for key, group := range e.state.Groups {
		if !strings.HasPrefix(key, prefix) || group == nil {
			continue
		}
		for _, ws := range group.Windows {
			if ws != nil {
				ws.Rebase = true
			}
		}
	}
	label := labelFor(providers.Title(cred.Provider), cred)
	e.addEventDetail("info", "plan_change", "", label, from+" → "+to, fmt.Sprintf("%s 套餐变化：%s → %s", label, from, to),
		map[string]string{"from": from, "to": to})
	// A change back before the first one was delivered cancels both.
	if n := state.Notice; n != nil {
		from = n.From
	}
	state.Notice = &store.PlanNotice{From: from, To: to, At: now.Unix()}
	if from == to {
		state.Notice = nil
	}
}

// sendPlanNotice sends the pending plan change notification of cred, and
// retries it with every active query for planNoticeAge after a failure.
func (e *Engine) sendPlanNotice(ctx context.Context, cred *Cred, state *store.PlanState, now time.Time) {
	n := state.Notice
	if n == nil || e.alerts == nil || e.alerts.Sender == nil {
		return
	}
	if now.Sub(time.Unix(n.At, 0)) > planNoticeAge {
		state.Notice = nil
		e.dirty = true
		return
	}
	msg := notify.PlanMessage(e.language(), labelFor(providers.Title(cred.Provider), cred), n.From, n.To)
	switch err := e.alerts.Sender.Send(ctx, msg); {
	case errors.Is(err, notify.ErrNotConfigured):
		// Notifications are off.
	case err != nil:
		e.notifyFailed("", msg, err)
		return
	default:
		e.notified("", msg)
	}
	state.Notice = nil
	e.dirty = true
}

// applyGroups merges new windows into the current view, records history,
// updates the reset observation of ignition targets and, when alerts is
// set, runs the alerts. An active result replaces the group; a passive result only updates the
// windows it contains. Once an active query has described the group, a
// passive result does not add windows: response headers can carry a window
// the usage API does not report, such as a 5-hour window on a ChatGPT Pro
// account whose usage API reports only the weekly one. A new window then
// appears with the next active query.
func (e *Engine) applyGroups(ctx context.Context, cfg config.Config, cred *Cred, raw []quota.RawGroup, source quota.Source, now time.Time, alerts bool) {
	var changed []*GroupView
	e.mu.Lock()
	for _, rg := range raw {
		key := cred.Provider + ":" + cred.AuthIndex + ":" + rg.Key
		group, ok := e.groups[key]
		if !ok {
			group = &GroupView{Key: key, Provider: cred.Provider, AuthIndex: cred.AuthIndex}
			e.groups[key] = group
		}
		group.SourceLabel = rg.SourceLabel
		group.Label = labelFor(group.SourceLabel, cred)
		if source == quota.SourceActive {
			group.Windows = group.Windows[:0]
			group.active = true
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
			if !replaced && (source == quota.SourceActive || !group.active) {
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
		if e.alerts != nil && alerts {
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
	sent := notify.ProcessResetRecords(ctx, e.state, records, cfg.CodexResetUpdates.NotifyCurrentPending, sender, cfg.Location(), e.language(), e.now())
	if sent > 0 {
		e.addEventDetail("info", "codex_reset", "", "Did Codex Reset", fmt.Sprintf("已发送 %d 条通知", sent), fmt.Sprintf("Did Codex Reset：已发送 %d 条通知", sent),
			map[string]string{"count": strconv.Itoa(sent)})
	}
	e.dirty = true
}

var errNoTarget = errors.New("找不到该点火目标")
