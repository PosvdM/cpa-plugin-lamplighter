package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/ignite"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// maxCandidates limits how many models one ignition tries.
const maxCandidates = 6

// TargetView is the ignition status of one quota group.
type TargetView struct {
	Key                 string    `json:"key"`
	Label               string    `json:"label"`
	Provider            string    `json:"provider"`
	AuthIndex           string    `json:"auth_index"`
	NextDue             time.Time `json:"next_due"`
	Reset               time.Time `json:"reset,omitempty"`
	Rolling             bool      `json:"rolling"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastAttempt         time.Time `json:"last_attempt,omitempty"`
	LastModel           string    `json:"last_model,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CircuitUntil        time.Time `json:"circuit_until,omitempty"`
	CircuitReason       string    `json:"circuit_reason,omitempty"`
	// BlockedWindow names a used-up window that holds ignition back, and
	// BlockedUntil is its reset, zero when unknown.
	BlockedWindow string    `json:"blocked_window,omitempty"`
	BlockedUntil  time.Time `json:"blocked_until,omitempty"`
}

type target struct {
	group  GroupView
	cred   *Cred
	reset  time.Time
	source string
}

// groupIgnitable reports whether ignition is enabled for the group.
func groupIgnitable(cfg config.Config, provider, sourceLabel string) bool {
	p := cfg.Provider(provider)
	if !p.Ignite {
		return false
	}
	if len(p.Groups) == 0 {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(sourceLabel))
	for _, allowed := range p.Groups {
		if strings.ToLower(strings.TrimSpace(allowed)) == label {
			return true
		}
	}
	return false
}

// collectTargets returns the groups that have a 5-hour window and ignition
// enabled.
func (e *Engine) collectTargets(cfg config.Config) []target {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []target
	for _, group := range e.groups {
		cred := e.creds[group.AuthIndex]
		if cred == nil || !groupIgnitable(cfg, group.Provider, group.SourceLabel) {
			continue
		}
		w, ok := group.fiveHour()
		if !ok {
			continue
		}
		snapshot := *group
		snapshot.Windows = append([]WindowView(nil), group.Windows...)
		out = append(out, target{group: snapshot, cred: cred, reset: w.Reset, source: string(w.Source)})
	}
	sortTargets(out)
	return out
}

func sortTargets(targets []target) {
	for i := 1; i < len(targets); i++ {
		for j := i; j > 0 && targets[j].group.Label < targets[j-1].group.Label; j-- {
			targets[j], targets[j-1] = targets[j-1], targets[j]
		}
	}
}

func (e *Engine) nextDue(cfg config.Config) time.Time {
	if e.state == nil {
		return time.Time{}
	}
	schedule := e.schedule(cfg)
	now := e.now()
	var next time.Time
	for _, t := range e.collectTargets(cfg) {
		due := schedule.DueAt(e.state.Target(t.group.Key), t.reset, now)
		if w, blocked := exhaustedWindow(t.group, now); blocked {
			// Wake at the reset; the next poll shows whether the quota is back.
			if w.Reset.IsZero() {
				continue
			}
			due = w.Reset
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}

// refreshTargets rebuilds the ignition status shown on the page.
func (e *Engine) refreshTargets(cfg config.Config) {
	if e.state == nil {
		return
	}
	schedule := e.schedule(cfg)
	now := e.now()
	targets := e.collectTargets(cfg)
	views := make([]TargetView, 0, len(targets))
	for _, t := range targets {
		ts := e.state.Target(t.group.Key)
		view := TargetView{
			Key:                 t.group.Key,
			Label:               t.group.Label,
			Provider:            t.group.Provider,
			AuthIndex:           t.group.AuthIndex,
			Reset:               t.reset,
			Rolling:             ts.RollingReset,
			LastSuccess:         epochTime(ts.LastSuccessEpoch),
			LastAttempt:         epochTime(ts.LastAttemptEpoch),
			LastModel:           ts.LastModel,
			LastError:           ts.LastError,
			ConsecutiveFailures: ts.ConsecutiveFailures,
			CircuitReason:       ts.CircuitReason,
		}
		if until := epochTime(ts.CircuitOpenUntilEpoch); until.After(now) {
			view.CircuitUntil = until
		}
		if w, blocked := exhaustedWindow(t.group, now); blocked {
			view.BlockedWindow = quota.ShortLabel(w.Label)
			view.BlockedUntil = w.Reset
		} else if cfg.Ignition.Enabled {
			view.NextDue = schedule.DueAt(ts, t.reset, now)
		}
		views = append(views, view)
	}
	e.mu.Lock()
	e.targets = views
	e.mu.Unlock()
}

func epochTime(epoch float64) time.Time {
	if epoch <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(epoch*1e9)).UTC()
}

// performDue ignites every target whose due time has come.
func (e *Engine) performDue(ctx context.Context, cfg config.Config) bool {
	schedule := e.schedule(cfg)
	attempted := false
	for _, t := range e.collectTargets(cfg) {
		if ctx.Err() != nil {
			break
		}
		now := e.now()
		// A used-up window, such as the 7-day quota, rejects every request
		// until it resets; scheduled ignition skips the target until then.
		if _, blocked := exhaustedWindow(t.group, now); blocked {
			continue
		}
		due := schedule.DueAt(e.state.Target(t.group.Key), t.reset, now)
		if due.After(now.Add(250 * time.Millisecond)) {
			continue
		}
		e.igniteTarget(ctx, cfg, t, false)
		attempted = true
	}
	return attempted
}

// configuredModel returns the model override for the target, if any.
func configuredModel(cfg config.Config, provider, groupLabel string) string {
	p := cfg.Provider(provider)
	for key, value := range p.Models {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(groupLabel)) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return p.Model
}

// igniteTarget sends the ignition request, confirms it and records the
// outcome. It returns the error shown for a manual ignition.
func (e *Engine) igniteTarget(ctx context.Context, cfg config.Config, t target, manual bool) error {
	ts := e.state.Target(t.group.Key)
	schedule := e.schedule(cfg)
	start := e.now()
	ts.LastAttemptEpoch = float64(start.UnixNano()) / 1e9
	e.dirty = true

	model, resp, err := e.execute(ctx, cfg, t)
	if err == nil {
		err = e.confirm(ctx, cfg, t, start, resp)
	}
	now := e.now()
	if err != nil {
		outcome := schedule.RecordFailure(ts, now, err)
		e.dirty = true
		if outcome.CircuitOpened {
			until := outcome.Until.In(cfg.Location()).Format("01/02 15:04")
			e.addEventDetail("error", "ignite_paused", t.group.Key, t.group.Label,
				fmt.Sprintf("暂停到 %s：%v", until, err),
				fmt.Sprintf("%s 点火失败，暂停到 %s：%v", t.group.Label, until, err),
				map[string]string{"until": eventTime(outcome.Until), "error": err.Error()})
			if outcome.NotifyCircuit && e.alerts != nil && e.alerts.Sender != nil {
				msg := notify.CircuitMessage(e.language(), t.group.Label, outcome.Until, err.Error(), cfg.Location())
				if sendErr := e.alerts.Sender.Send(ctx, msg); sendErr != nil {
					e.notifyFailed(t.group.Key, msg, sendErr)
				} else {
					e.notified(t.group.Key, msg)
				}
			}
		} else {
			retry := retryText(outcome.RetryIn)
			if outcome.Cooldown {
				retry = "CPA 冷却结束，" + retry
			}
			params := map[string]string{"retry_seconds": strconv.Itoa(int(outcome.RetryIn.Round(time.Second).Seconds())), "error": err.Error()}
			if outcome.Cooldown {
				params["cooldown"] = "1"
			}
			e.addEventDetail("warn", "ignite_failed", t.group.Key, t.group.Label,
				fmt.Sprintf("%s后重试：%v", retry, err),
				fmt.Sprintf("%s 点火失败，%s后重试：%v", t.group.Label, retry, err), params)
		}
		return err
	}
	ignite.RecordSuccess(ts, model, now)
	e.dirty = true
	resetText := "未知"
	params := map[string]string{"model": model}
	if group, ok := e.group(t.group.Key); ok {
		if w, ok := group.fiveHour(); ok && !w.Reset.IsZero() {
			resetText = w.Reset.In(cfg.Location()).Format("01/02 15:04:05")
			params["reset"] = eventTime(w.Reset)
		}
	}
	kind := "ignite"
	if manual {
		kind = "ignite_manual"
	}
	e.addEventDetail("info", kind, t.group.Key, t.group.Label,
		fmt.Sprintf("%s · 下次重置 %s", model, resetText),
		fmt.Sprintf("%s 点火成功（%s），下次重置 %s", t.group.Label, model, resetText), params)
	return nil
}

func (e *Engine) group(key string) (GroupView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	group, ok := e.groups[key]
	if !ok {
		return GroupView{}, false
	}
	snapshot := *group
	snapshot.Windows = append([]WindowView(nil), group.Windows...)
	return snapshot, true
}

// execute sends the ignition request with the newest candidate model. It
// tries the next candidate when CPA rejects a model for the pinned
// credential, and once per ignition when the provider refuses a model.
func (e *Engine) execute(ctx context.Context, cfg config.Config, t target) (string, pluginapi.HostModelExecutionResponse, error) {
	var empty pluginapi.HostModelExecutionResponse
	override := configuredModel(cfg, t.group.Provider, t.group.SourceLabel)
	ts := e.state.Target(t.group.Key)

	available, err := e.lister.List(ctx, cfg.CPABaseURL, cfg.ModelsAPIKey)
	e.noteModels(available, err)
	var candidates []string
	switch {
	case err != nil && override != "":
		candidates = []string{override}
	case err != nil && ts.LastModel != "":
		// Keep igniting with the last working model while the list is unavailable.
		candidates = []string{ts.LastModel}
	case err != nil:
		return "", empty, err
	default:
		candidates, err = models.Candidates(t.group.Provider, t.group.SourceLabel, override, available)
		if err != nil {
			return "", empty, fmt.Errorf("%w: %v", ignite.ErrNoModel, err)
		}
	}
	if override == "" {
		candidates = e.skipRefused(t.group.Key, candidates)
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	var lastErr, refusedErr error
	refused := ""
	for i, model := range candidates {
		req, err := ignite.BuildRequest(t.group.Provider, model)
		if err != nil {
			return "", empty, err
		}
		resp, err := e.host.ModelExecute(pluginapi.HostModelExecutionRequest{
			EntryProtocol:  req.EntryProtocol,
			ExitProtocol:   req.EntryProtocol,
			Model:          model,
			Body:           req.Body,
			ForcedProvider: t.group.Provider,
			AuthID:         t.cred.ID,
		})
		if err == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			err = &host.Error{
				Method:  "host.model.execute",
				Code:    "model_execution_failed",
				Message: truncateText(string(resp.Body), 300),
				Status:  resp.StatusCode,
			}
		}
		if err == nil {
			if refused != "" {
				e.noteRefused(t, refused, model)
			}
			return model, resp, nil
		}
		if ignite.Classify(err) == ignite.Unsupported {
			lastErr = err
			continue
		}
		// CPA lists a new model for every credential of the provider, and one
		// account may not be able to use it yet. A second refusal points at
		// the request or the account, so it ends the ignition.
		if refused == "" && i < len(candidates)-1 && ignite.ModelRejected(err) {
			refused, refusedErr = model, err
			continue
		}
		if refused != "" {
			// Only the status of the refusal is added: its text could carry
			// markers that change how the final error is classified.
			status := ""
			if code := host.StatusOf(refusedErr); code != 0 {
				status = fmt.Sprintf("（状态码 %d）", code)
			}
			err = fmt.Errorf("%w；此前 %s 被上游拒绝%s", err, refused, status)
		}
		return model, resp, err
	}
	if refused != "" {
		return "", empty, fmt.Errorf("%w（%s 被上游拒绝：%v；最后一次：%v）", ignite.ErrNoModel, refused, refusedErr, lastErr)
	}
	return "", empty, fmt.Errorf("%w（最后一次：%v）", ignite.ErrNoModel, lastErr)
}

// refusedFor is how long a model the provider refused for a quota group is
// skipped. A new model can reach accounts in stages, so it is tried again
// after a day instead of being dropped; a day keeps requests for a model the
// account cannot use rare.
const refusedFor = 24 * time.Hour

// noteRefused remembers that the provider refused model for the target while
// fallback worked, which shows the refusal concerns the model.
func (e *Engine) noteRefused(t target, model, fallback string) {
	if e.refused == nil {
		e.refused = map[string]time.Time{}
	}
	e.refused[t.group.Key+"\x00"+model] = e.now()
	hours := strconv.Itoa(int(refusedFor / time.Hour))
	e.addEventDetail("warn", "model_refused", t.group.Key, t.group.Label,
		fmt.Sprintf("%s 被上游拒绝，改用 %s，%s 小时内不再尝试", model, fallback, hours),
		fmt.Sprintf("%s 点火模型 %s 被上游拒绝，改用 %s，%s 小时内不再尝试", t.group.Label, model, fallback, hours),
		map[string]string{"model": model, "fallback": fallback, "hours": hours})
}

// skipRefused drops the candidates refused for the target within refusedFor,
// unless that leaves none, and forgets expired refusals.
func (e *Engine) skipRefused(key string, candidates []string) []string {
	now := e.now()
	for k, at := range e.refused {
		if now.Sub(at) >= refusedFor {
			delete(e.refused, k)
		}
	}
	var out []string
	for _, model := range candidates {
		if _, ok := e.refused[key+"\x00"+model]; !ok {
			out = append(out, model)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

// nextModelGroups is the quota group whose automatic model the settings show
// for each provider. Antigravity shows the Gemini group, which matches the
// "newest Flash" description.
var nextModelGroups = map[string]string{"codex": "", "claude": "", "antigravity": "Gemini"}

// maybeReadModels reads the CPA model list after a start and after a change
// of the CPA address or models key, so that the settings show the next
// automatic model without waiting for an ignition. Otherwise the list is only
// read for an ignition. A failed read is retried with the next poll.
func (e *Engine) maybeReadModels(ctx context.Context, cfg config.Config, pollDue bool) {
	source := ""
	if cfg.ModelsAPIKey != "" {
		source = cfg.CPABaseURL + "\x00" + cfg.ModelsAPIKey
	}
	if source != e.modelsSource {
		// Models and errors of another CPA or key do not describe the next
		// ignition.
		e.modelsSource, e.modelsTried, e.modelsOK = source, false, false
		e.mu.Lock()
		e.nextModels, e.modelsErr = nil, ""
		e.mu.Unlock()
	}
	if source == "" || e.modelsOK || (e.modelsTried && !pollDue) {
		return
	}
	e.modelsTried = true
	available, err := e.lister.List(ctx, cfg.CPABaseURL, cfg.ModelsAPIKey)
	e.noteModels(available, err)
	e.modelsOK = err == nil
}

// noteModels records the result of reading the model list and the first
// automatic candidate of each provider. A failed read keeps the last known
// models.
func (e *Engine) noteModels(available []string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.modelsErr = err.Error()
		return
	}
	e.modelsErr = ""
	next := make(map[string]string, len(nextModelGroups))
	for provider, group := range nextModelGroups {
		if candidates, err := models.Candidates(provider, group, "", available); err == nil {
			next[provider] = candidates[0]
		}
	}
	e.nextModels = next
}

func truncateText(text string, n int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n])
}

// confirmed reports whether the target's 5-hour window has a fixed future
// reset observed after start.
func (e *Engine) confirmed(key string, start, now time.Time) bool {
	group, ok := e.group(key)
	if !ok {
		return false
	}
	w, ok := group.fiveHour()
	if !ok || w.ObservedAt.Before(start) || w.Reset.IsZero() || !w.Reset.After(now) {
		return false
	}
	return !e.state.Target(key).RollingReset
}

// confirm checks that the ignition started a window. Claude responses carry
// the new reset in their headers; Codex reports it through the usage record
// of the same request. When neither arrives, and for Antigravity, the
// credential is queried actively.
func (e *Engine) confirm(ctx context.Context, cfg config.Config, t target, start time.Time, resp pluginapi.HostModelExecutionResponse) error {
	if groups := quota.ParsePassive(t.group.Provider, resp.Headers, e.now()); len(groups) > 0 {
		now := e.now()
		e.passiveAt[t.cred.AuthIndex] = now
		e.notePassiveFable(t.cred, groups)
		e.applyGroups(ctx, cfg, t.cred, groups, quota.SourcePassive, now)
	}
	if e.confirmed(t.group.Key, start, e.now()) {
		return nil
	}
	for _, delay := range confirmDelays {
		if !e.waitWithUsage(ctx, delay) {
			return context.Canceled
		}
		if e.confirmed(t.group.Key, start, e.now()) {
			return nil
		}
		if err := e.refreshCred(ctx, cfg, t.cred); err != nil {
			e.logf("warn", "点火确认时刷新额度失败：%s：%v", t.group.Label, err)
		}
		if e.confirmed(t.group.Key, start, e.now()) {
			return nil
		}
	}
	return ignite.ErrNotConfirmed
}

// retryText formats a retry delay in seconds below one minute, in minutes
// otherwise.
func retryText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Round(time.Minute).Minutes()))
}
