package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/providers"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// MCPBackend exposes a separate, model-safe view of the engine.
type MCPBackend struct {
	engine      *Engine
	mu          sync.Mutex
	nextRefresh time.Time
}

func NewMCPBackend(e *Engine) *MCPBackend { return &MCPBackend{engine: e} }

type modelTime struct {
	ISO      string `json:"iso"`
	Relative string `json:"relative"`
}

func describeTime(t, now time.Time) *modelTime {
	if t.IsZero() {
		return nil
	}
	d := t.Sub(now).Round(time.Second)
	relative := "in " + d.String()
	if d < 0 {
		relative = (-d).String() + " ago"
	}
	return &modelTime{t.UTC().Format(time.RFC3339), relative}
}
func accountLabel(a AccountStatus, index int) string {
	if name := strings.TrimSpace(strings.SplitN(a.Note, "\n", 2)[0]); name != "" {
		return name
	}
	if a.Email != "" {
		return a.Email
	}
	return fmt.Sprintf("account-%d", index+1)
}
func matchesAccount(a AccountStatus, index int, filter string) bool {
	return filter == "" || filter == accountLabel(a, index) || a.Email != "" && filter == a.Email
}
func safeQueryError(a AccountStatus) string {
	if a.Error == "" {
		return ""
	}
	text := strings.ToLower(a.Error)
	if strings.Contains(text, "401") || strings.Contains(text, "unauthorized") || strings.Contains(text, "invalid_grant") {
		return "credential authentication failed"
	}
	if strings.Contains(text, "429") {
		return "quota query rate limited"
	}
	return "quota query failed"
}
func selectedAccounts(s Status, args map[string]string) map[string]string {
	out := map[string]string{}
	for i, a := range s.Accounts {
		if (args["provider"] == "" || args["provider"] == a.Provider) && matchesAccount(a, i, args["account"]) {
			out[a.AuthIndex] = accountLabel(a, i)
		}
	}
	return out
}
func (b *MCPBackend) CallTool(ctx context.Context, name string, args map[string]string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch name {
	case "list_quotas":
		return b.listContext(ctx, args)
	case "get_quota_history":
		return b.history(ctx, args)
	case "refresh_quotas":
		return b.refresh(ctx, args)
	case "get_quota_events":
		return b.events(ctx, args)
	}
	return nil, errors.New("Unknown tool")
}

type quotaWindowDTO struct {
	ID          string     `json:"window"`
	Kind        string     `json:"kind,omitempty"`
	Remaining   float64    `json:"remaining_percent"`
	Reset       *modelTime `json:"reset"`
	Started     *bool      `json:"started"`
	Rate        *float64   `json:"consumption_percent_per_hour"`
	Exhaustion  *modelTime `json:"estimated_exhaustion"`
	BeforeReset *bool      `json:"exhausts_before_reset"`
	Pace        any        `json:"seven_day_pace,omitempty"`
	Updated     *modelTime `json:"updated"`
	AgeSeconds  int64      `json:"age_seconds"`
	Stale       bool       `json:"stale"`
	Source      string     `json:"source"`
}
type quotaAccountDTO struct {
	Account           string           `json:"account"`
	Plan              string           `json:"plan"`
	Weight            float64          `json:"weight"`
	Unavailable       bool             `json:"unavailable"`
	CredentialInvalid bool             `json:"credential_invalid"`
	Error             string           `json:"recent_error,omitempty"`
	Cooldown          *modelTime       `json:"cooldown_until"`
	NextIgnition      *modelTime       `json:"next_ignition"`
	Windows           []quotaWindowDTO `json:"windows"`
}
type aggregateDTO struct {
	Window       string     `json:"window"`
	Remaining    float64    `json:"weighted_remaining_percent"`
	FirstAccount string     `json:"first_exhausting_account,omitempty"`
	Exhaustion   *modelTime `json:"earliest_estimated_exhaustion"`
	weight       float64
	earliest     time.Time
}
type quotaGroupDTO struct {
	Provider string            `json:"provider"`
	Group    string            `json:"group"`
	Accounts []quotaAccountDTO `json:"accounts"`
	Totals   []*aggregateDTO   `json:"totals"`
	byWindow map[string]*aggregateDTO
}

func (b *MCPBackend) list(args map[string]string) (any, error) {
	return b.listContext(context.Background(), args)
}

func (b *MCPBackend) listContext(ctx context.Context, args map[string]string) (any, error) {
	e := b.engine
	s := e.Status()
	selected := selectedAccounts(s, args)
	currentSeries := map[string]bool{}
	for _, account := range s.Accounts {
		if _, ok := selected[account.AuthIndex]; !ok {
			continue
		}
		for _, group := range account.Groups {
			for _, window := range group.Windows {
				currentSeries[group.Key+"|"+window.ID] = true
			}
		}
	}
	history, historyErr := e.mcpHistoryRange(ctx, "8d", "", func(record store.Record) bool {
		return currentSeries[record.Group+"|"+record.Window]
	})
	if historyErr != nil {
		return nil, safeHistoryError(historyErr)
	}
	samples := map[string][][3]float64{}
	for _, series := range history.Series {
		samples[series.Group+"|"+series.Window] = series.Points
	}
	targets := map[string]TargetView{}
	for _, t := range s.Targets {
		targets[t.Key] = t
	}
	startStates := map[string][2]bool{}
	e.mu.Lock()
	for key, g := range e.groups {
		startStates[key] = [2]bool{g.startKnown, g.rolling}
	}
	e.mu.Unlock()
	groups := []*quotaGroupDTO{}
	withoutWindows := []any{}
	byGroup := map[string]*quotaGroupDTO{}
	for _, a := range s.Accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		label, ok := selected[a.AuthIndex]
		if !ok {
			continue
		}
		weight := s.Config.AccountWeights[a.Name]
		if weight <= 0 {
			weight = a.DefaultWeight
		}
		if weight <= 0 {
			weight = 1
		}
		hasWindows := false
		for _, g := range a.Groups {
			if len(g.Windows) == 0 {
				continue
			}
			hasWindows = true
			// CPA applies aliases, request affinity and per-model credential cooldowns
			// during live selection. No SDK callback exposes that decision here, so
			// a model query retains every candidate instead of guessing from its name.
			key := a.Provider + "|" + g.SourceLabel
			group := byGroup[key]
			if group == nil {
				group = &quotaGroupDTO{Provider: a.Provider, Group: g.SourceLabel, byWindow: map[string]*aggregateDTO{}}
				byGroup[key] = group
				groups = append(groups, group)
			}
			queryErr := safeQueryError(a)
			account := quotaAccountDTO{Account: label, Plan: a.Plan, Weight: weight, Unavailable: a.Unavailable, CredentialInvalid: queryErr == "credential authentication failed", Error: queryErr, Cooldown: describeTime(a.CooldownUntil, s.Now), NextIgnition: describeTime(targets[g.Key].NextDue, s.Now), Windows: []quotaWindowDTO{}}
			for _, w := range g.Windows {
				target := targets[g.Key]
				target.Rolling = startStates[g.Key][1]
				dto, err := forecastWindowContext(ctx, w, target, samples[g.Key+"|"+w.ID], s.Now, s.Config.PollInterval())
				if err != nil {
					return nil, err
				}
				if w.Kind == quota.KindFiveHour && !startStates[g.Key][0] {
					dto.Started = nil
					dto.Rate = nil
					dto.Exhaustion = nil
					dto.BeforeReset = nil
				}
				account.Windows = append(account.Windows, dto)
				total := group.byWindow[w.ID]
				if total == nil {
					total = &aggregateDTO{Window: w.ID}
					group.byWindow[w.ID] = total
					group.Totals = append(group.Totals, total)
				}
				total.Remaining += weight * w.Remaining
				total.weight += weight
				if dto.Exhaustion != nil {
					t, _ := time.Parse(time.RFC3339, dto.Exhaustion.ISO)
					if total.earliest.IsZero() || t.Before(total.earliest) {
						total.earliest = t
						total.FirstAccount = label
						total.Exhaustion = dto.Exhaustion
					}
				}
			}
			group.Accounts = append(group.Accounts, account)
		}
		if !hasWindows {
			withoutWindows = append(withoutWindows, map[string]any{"account": label, "provider": a.Provider, "plan": a.Plan, "weight": weight, "unavailable": a.Unavailable, "credential_invalid": safeQueryError(a) == "credential authentication failed", "recent_error": safeQueryError(a)})
		}
	}
	for _, g := range groups {
		for _, total := range g.Totals {
			total.Remaining /= total.weight
		}
	}
	return map[string]any{"as_of": describeTime(s.Now, s.Now), "model": args["model"], "certain": args["model"] == "", "routing_note": "Model requests list conservative candidate pools: CPA aliases, affinity, exclusions and live credential selection cannot be resolved through the plugin SDK.", "forecast_note": "Rates are percentage points/hour. Forecasts assume unchanged consumption; null means unknown or stale. Seven-day rate is this cycle's average; five-hour rate is a recent piecewise linear fit.", "groups": groups, "accounts_without_windows": withoutWindows}, nil
}

func forecastWindow(w WindowStatus, target TargetView, points [][3]float64, now time.Time, poll time.Duration) quotaWindowDTO {
	dto, _ := forecastWindowContext(context.Background(), w, target, points, now, poll)
	return dto
}

func forecastWindowContext(ctx context.Context, w WindowStatus, target TargetView, points [][3]float64, now time.Time, poll time.Duration) (quotaWindowDTO, error) {
	if err := ctx.Err(); err != nil {
		return quotaWindowDTO{}, err
	}
	age := max(int64(0), int64(now.Sub(w.ObservedAt).Seconds()))
	freshLimit := max(15*time.Minute, 3*poll)
	dto := quotaWindowDTO{ID: w.ID, Kind: w.Kind, Remaining: w.Remaining, Reset: describeTime(w.Reset, now), Updated: describeTime(w.ObservedAt, now), AgeSeconds: age, Stale: w.ObservedAt.IsZero() || now.Sub(w.ObservedAt) > freshLimit, Source: w.Source}
	period := time.Duration(w.PeriodSeconds * float64(time.Second))
	if w.Kind == quota.KindFiveHour {
		period = quota.FiveHours
	}
	if w.Kind == quota.KindSevenDay {
		period = quota.SevenDays
	}
	if period == 0 || w.Reset.IsZero() {
		return dto, nil
	}
	start := w.Reset.Add(-period)
	started := now.After(start) && now.Before(w.Reset)
	if w.Kind == quota.KindFiveHour && target.Rolling {
		started = false
	}
	dto.Started = &started
	if !started {
		return dto, nil
	}
	if w.Kind == quota.KindSevenDay {
		expected := min(100.0, max(0.0, 100*w.Reset.Sub(now).Seconds()/period.Seconds()))
		dto.Pace = map[string]float64{"expected_remaining_percent": expected, "actual_remaining_percent": w.Remaining, "difference_percent": w.Remaining - expected, "even_budget_percent_per_day": 100.0 / 7, "remaining_budget_percent_per_day": w.Remaining / (w.Reset.Sub(now).Hours() / 24)}
	}
	if dto.Stale {
		return dto, nil
	}
	if w.Remaining <= notify.ExhaustedRemaining {
		dto.Exhaustion = describeTime(now, now)
		before := true
		dto.BeforeReset = &before
	}
	rate := 0.0
	base := now
	if w.Kind == quota.KindSevenDay {
		rate = max(0.0, (100-w.Remaining)/now.Sub(start).Hours())
	} else {
		var floor [][3]float64
		for _, p := range points {
			if err := ctx.Err(); err != nil {
				return dto, err
			}
			if p[0] < float64(start.Unix()) || p[0] > float64(w.ObservedAt.Unix()) {
				continue
			}
			if len(floor) > 0 {
				prev := floor[len(floor)-1][1]
				if p[1] >= prev+5 {
					floor = nil
				} else {
					p[1] = min(prev, p[1])
				}
			}
			floor = append(floor, p)
		}
		if len(floor) < 2 {
			return dto, nil
		}
		// Bound the piecewise fit independently of the retained history size.
		if len(floor) > 256 {
			floor = downsample(floor, 256)
		}
		last := floor[len(floor)-1]
		hold := max(2*time.Minute, poll*3/2)
		if last[2] == 1 {
			hold = 2 * time.Minute
		}
		base = time.Unix(int64(last[0]), 0)
		if now.Sub(base) > hold {
			floor = append(floor, [3]float64{float64(now.Unix()), last[1], last[2]})
			base = now
		}
		slope, err := recentSlopeContext(ctx, floor)
		if err != nil {
			return dto, err
		}
		rate = max(0.0, -slope*3600)
	}
	dto.Rate = &rate
	before := false
	dto.BeforeReset = &before
	if w.Remaining <= notify.ExhaustedRemaining {
		base = now
	} else if rate > 0 {
		seconds := w.Remaining / rate * 3600
		if seconds > float64((365*24*time.Hour)/time.Second) {
			return dto, nil
		}
		base = base.Add(time.Duration(seconds * float64(time.Second)))
	} else {
		return dto, nil
	}
	dto.Exhaustion = describeTime(base, now)
	before = base.Before(w.Reset)
	dto.BeforeReset = &before
	return dto, nil
}

func lineFit(points [][3]float64, a, b int) (float64, float64) {
	n, t0 := float64(b-a+1), points[a][0]
	var st, sv, stt, stv float64
	for i := a; i <= b; i++ {
		t := points[i][0] - t0
		st += t
		sv += points[i][1]
		stt += t * t
		stv += t * points[i][1]
	}
	slope := 0.0
	if d := n*stt - st*st; d != 0 {
		slope = (n*stv - st*sv) / d
	}
	base := (sv - slope*st) / n
	err := 0.0
	for i := a; i <= b; i++ {
		err = max(err, math.Abs(base+slope*(points[i][0]-t0)-points[i][1]))
	}
	return slope, err
}
func recentSlope(points [][3]float64) float64 {
	slope, _ := recentSlopeContext(context.Background(), points)
	return slope
}

func recentSlopeContext(ctx context.Context, points [][3]float64) (float64, error) {
	if len(points) < 2 {
		return 0, ctx.Err()
	}
	pieces := make([][2]int, len(points)-1)
	for i := range pieces {
		pieces[i] = [2]int{i, i + 1}
	}
	for len(pieces) > 1 {
		best, cost := 0, math.Inf(1)
		for i := 0; i < len(pieces)-1; i++ {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			_, err := lineFit(points, pieces[i][0], pieces[i+1][1])
			if err < cost {
				best, cost = i, err
			}
		}
		if cost > 2 {
			break
		}
		pieces[best][1] = pieces[best+1][1]
		pieces = append(pieces[:best+1], pieces[best+2:]...)
	}
	last := pieces[len(pieces)-1]
	slope, _ := lineFit(points, last[0], last[1])
	return slope, ctx.Err()
}
func downsample(points [][3]float64, limit int) [][3]float64 {
	if len(points) <= limit {
		return points
	}
	out := make([][3]float64, limit)
	for i := range out {
		out[i] = points[i*(len(points)-1)/(limit-1)]
	}
	return out
}

func safeHistoryError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, store.ErrHistoryLimit) {
		return err
	}
	return errors.New("Quota history is unavailable")
}

func (b *MCPBackend) history(ctx context.Context, args map[string]string) (any, error) {
	if args["range"] != "" {
		if _, ok := historyRanges[args["range"]]; !ok && args["range"] != "1mo" {
			return nil, errors.New("Unsupported history range")
		}
	}
	s := b.engine.Status()
	selected := selectedAccounts(s, args)
	history, err := b.engine.mcpHistoryRange(ctx, args["range"], args["window"], func(record store.Record) bool {
		parts := strings.SplitN(record.Group, ":", 3)
		if len(parts) != 3 || providers.Lookup(parts[0]) == nil || args["provider"] != "" && args["provider"] != parts[0] {
			return false
		}
		window := args["window"]
		if window != "" && window != "5h" && window != "7d" && record.Window != window {
			return false
		}
		if args["account"] == "" {
			return true
		}
		if _, ok := selected[parts[1]]; ok {
			return true
		}
		if args["account"] != "retired account" {
			return false
		}
		for _, account := range s.Accounts {
			if account.Provider == parts[0] && account.AuthIndex == parts[1] {
				return false
			}
		}
		return true
	})
	if err != nil {
		return nil, safeHistoryError(err)
	}
	// Match historical group keys against known accounts without exposing them.
	type historical struct {
		Account  string `json:"account"`
		Provider string `json:"provider"`
		Group    string `json:"group"`
		Window   string `json:"window"`
		Points   []any  `json:"points"`
	}
	out := []historical{}
	remaining := 2000
	for _, series := range history.Series {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		label, ok := selected[series.AuthIndex]
		provider := series.Provider
		groupLabel := series.SourceLabel
		if !ok {
			for _, a := range s.Accounts {
				if candidate, exists := selected[a.AuthIndex]; exists && strings.HasPrefix(series.Group, a.Provider+":"+a.AuthIndex+":") {
					label, ok, provider = candidate, true, a.Provider
					groupLabel = "retired quota pool"
					break
				}
			}
		}
		if !ok && (args["account"] == "" || args["account"] == "retired account") {
			parts := strings.SplitN(series.Group, ":", 3)
			if len(parts) == 3 && providers.Lookup(parts[0]) != nil && (args["provider"] == "" || args["provider"] == parts[0]) {
				known := false
				for _, a := range s.Accounts {
					if a.Provider == parts[0] && a.AuthIndex == parts[1] {
						known = true
						break
					}
				}
				if !known {
					label, provider, groupLabel, ok = "retired account", parts[0], "retired quota pool", true
				}
			}
		}
		if groupLabel == "" {
			groupLabel = "retired quota pool"
		}
		if !ok || remaining < 2 || (args["window"] != "" && args["window"] != "5h" && args["window"] != "7d" && series.Window != args["window"]) {
			continue
		}
		points := downsample(series.Points, min(200, remaining))
		remaining -= len(points)
		row := historical{Account: label, Provider: provider, Group: groupLabel, Window: series.Window, Points: []any{}}
		for _, p := range points {
			source := "active"
			if p[2] == 1 {
				source = "passive"
			}
			row.Points = append(row.Points, map[string]any{"time": describeTime(time.Unix(int64(p[0]), 0), s.Now), "remaining_percent": p[1], "source": source})
		}
		out = append(out, row)
	}
	return map[string]any{"from": describeTime(history.From, s.Now), "to": describeTime(history.To, s.Now), "max_points": 2000, "series": out}, nil
}
func (b *MCPBackend) refresh(ctx context.Context, args map[string]string) (any, error) {
	s := b.engine.Status()
	auth := ""
	if args["account"] != "" {
		selected := selectedAccounts(s, args)
		if len(selected) != 1 {
			return nil, errors.New("Account must match exactly one display name or email")
		}
		for id := range selected {
			auth = id
		}
	}
	b.mu.Lock()
	now := b.engine.now()
	if b.nextRefresh.After(now) {
		seconds := int(math.Ceil(b.nextRefresh.Sub(now).Seconds()))
		b.mu.Unlock()
		return map[string]any{"ok": false, "retry_after_seconds": seconds, "retry_at": describeTime(now.Add(time.Duration(seconds)*time.Second), now)}, nil
	}
	b.nextRefresh = now.Add(time.Minute)
	b.mu.Unlock()
	e := b.engine
	e.mu.Lock()
	ready := e.running && e.enabled
	e.mu.Unlock()
	if !ready {
		return nil, errors.New("Quota engine is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := command{kind: "refresh", arg: auth, reply: make(chan commandReply, 1)}
	select {
	case e.cmdCh <- cmd:
	case <-ctx.Done():
		return nil, errors.New("Refresh wait timed out or was canceled")
	case <-e.stopCh:
		return nil, errors.New("Quota engine stopped")
	}
	select {
	case result := <-cmd.reply:
		if result.err != nil {
			return nil, errors.New("Quota refresh failed; read list_quotas for per-account state")
		}
		return map[string]any{"ok": true, "as_of": describeTime(e.now(), e.now())}, nil
	case <-ctx.Done():
		return nil, errors.New("Refresh wait timed out or was canceled; queued work may still complete")
	case <-e.stopCh:
		return nil, errors.New("Quota engine stopped")
	}
}
func (b *MCPBackend) events(ctx context.Context, args map[string]string) (any, error) {
	since := time.Time{}
	if args["since"] != "" {
		var err error
		since, err = time.Parse(time.RFC3339, args["since"])
		if err != nil {
			return nil, errors.New("since must be an RFC3339 timestamp")
		}
	}
	limit := 50
	if args["limit"] != "" {
		n, err := strconv.Atoi(args["limit"])
		if err != nil || n < 1 || n > 100 {
			return nil, errors.New("limit must be 1-100")
		}
		limit = n
	}
	s := b.engine.Status()
	labels := map[string][2]string{}
	for i, a := range s.Accounts {
		for _, g := range a.Groups {
			labels[g.Key] = [2]string{accountLabel(a, i), g.SourceLabel}
		}
	}
	out := []any{}
	// Return typed events without titles, raw errors or arbitrary event text.
	b.engine.mu.Lock()
	events := append([]Event(nil), b.engine.events...)
	b.engine.mu.Unlock()
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.After(events[j].Time) })
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Time.Before(since) {
			continue
		}
		switch event.Kind {
		case "reset", "exhausted", "ignite", "ignite_manual", "ignite_failed", "ignite_paused":
		default:
			continue
		}
		label, ok := labels[event.Group]
		if !ok {
			continue
		}
		out = append(out, map[string]any{"time": describeTime(event.Time, s.Now), "kind": event.Kind, "account": label[0], "group": label[1], "level": event.Level, "window": event.Params["window"], "reset": describeTime(quota.ParseTime(event.Params["reset"]), s.Now)})
		if len(out) >= limit {
			break
		}
	}
	return map[string]any{"events": out, "retention_note": "At most 200 recent engine events of all kinds are retained."}, nil
}

// recordQuotaTransitions is independent of notification delivery. Missing or
// rebased windows produce no transitions when a subscription changes.
func (e *Engine) recordQuotaTransitions(group *GroupView, previous []WindowView, now time.Time) {
	for _, w := range group.Windows {
		if !w.ObservedAt.Equal(now) {
			continue
		}
		if e.state != nil {
			if stored := e.state.Groups[group.Key]; stored != nil {
				if ws := stored.Windows[w.ID]; ws != nil && ws.Rebase {
					continue
				}
			}
		}
		for _, old := range previous {
			if old.ID != w.ID {
				continue
			}
			kind := ""
			rise := w.Remaining-old.Remaining >= notify.RecoveryJump
			resetMoved := !old.Reset.IsZero() && !w.Reset.IsZero() && !notify.SameResetCycle(eventTime(old.Reset), eventTime(w.Reset))
			if rise || resetMoved && !old.Reset.After(now) {
				kind = "reset"
			} else if old.Remaining > notify.ExhaustedRemaining && w.Remaining <= notify.ExhaustedRemaining {
				kind = "exhausted"
			}
			if kind != "" {
				description := "额度重置"
				if kind == "exhausted" {
					description = "额度耗尽"
				}
				detail := w.ID + " " + description
				params := map[string]string{"window": w.ID, "remaining": strconv.FormatFloat(w.Remaining, 'f', 2, 64)}
				if !w.Reset.IsZero() {
					params["reset"] = eventTime(w.Reset)
				}
				e.addEventDetail("info", kind, group.Key, group.Label, detail, group.Label+" "+detail, params)
			}
			break
		}
	}
}
