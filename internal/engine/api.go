package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// commandTimeout bounds how long a management request waits for the loop.
const commandTimeout = 3 * time.Minute

// Event is one entry of the event log.
type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Group   string    `json:"group,omitempty"`
	Message string    `json:"message"`
	// Label and Detail split Message into the quota group and the rest,
	// so the page can show events in columns.
	Label  string `json:"label,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Params holds the values of the event, such as the model or the retry
	// delay, so the page can write the event in its own language. Message
	// and Detail stay in Chinese for the CPA log and older events.
	Params map[string]string `json:"params,omitempty"`
}

func (e *Engine) addEvent(level, kind, group, message string) {
	e.addEventDetail(level, kind, group, "", message, message, nil)
}

// addEventDetail records an event. message is the full text for the CPA log;
// label and detail are the quota group and a short description for the page,
// which shows the event type separately.
func (e *Engine) addEventDetail(level, kind, group, label, detail, message string, params map[string]string) {
	now := e.now()
	e.mu.Lock()
	e.events = append(e.events, Event{Time: now, Level: level, Kind: kind, Group: group, Message: message, Label: label, Detail: detail, Params: params})
	if len(e.events) > maxEvents {
		e.events = e.events[len(e.events)-maxEvents:]
	}
	e.mu.Unlock()
	if e.history != nil {
		e.history.Append(store.Record{Kind: "e", Time: now.Unix(), Group: group, Event: kind, Level: level, Message: message, Label: label, Detail: detail, Params: params})
	}
	e.logf(level, "%s", message)
}

func (e *Engine) loadRecentEvents() {
	now := e.now()
	records, err := e.history.Query(now.Add(-7*24*time.Hour), now)
	if err != nil {
		return
	}
	var events []Event
	for _, record := range records {
		if record.Kind != "e" {
			continue
		}
		events = append(events, Event{
			Time:    time.Unix(record.Time, 0).UTC(),
			Level:   record.Level,
			Kind:    record.Event,
			Group:   record.Group,
			Message: record.Message,
			Label:   record.Label,
			Detail:  record.Detail,
			Params:  record.Params,
		})
	}
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	e.mu.Lock()
	e.events = append(events, e.events...)
	e.mu.Unlock()
}

type command struct {
	kind  string
	arg   string
	reply chan commandReply
}

type commandReply struct {
	err error
	// results is the outcome per channel of test_notify.
	results []ChannelResult
}

// ErrNotRunning means the background loop has not started yet.
var ErrNotRunning = errors.New("Lamplighter 尚未就绪：CPA 启动后约 20 秒开始运行，或插件已在配置中停用")

func (e *Engine) send(kind, arg string) error {
	return e.dispatch(kind, arg).err
}

// dispatch hands a command to the loop and waits for its reply.
func (e *Engine) dispatch(kind, arg string) commandReply {
	e.mu.Lock()
	ready := e.running && e.enabled
	e.mu.Unlock()
	if !ready {
		return commandReply{err: ErrNotRunning}
	}
	cmd := command{kind: kind, arg: arg, reply: make(chan commandReply, 1)}
	timer := time.NewTimer(commandTimeout)
	defer timer.Stop()
	select {
	case e.cmdCh <- cmd:
	case <-timer.C:
		return commandReply{err: errors.New("后台任务繁忙，请稍后再试")}
	case <-e.stopCh:
		return commandReply{err: ErrNotRunning}
	}
	select {
	case reply := <-cmd.reply:
		return reply
	case <-timer.C:
		return commandReply{err: errors.New("操作超时")}
	case <-e.stopCh:
		return commandReply{err: ErrNotRunning}
	}
}

// Refresh runs an active query now, for one credential or for all when
// authIndex is empty.
func (e *Engine) Refresh(authIndex string) error { return e.send("refresh", authIndex) }

// Ignite ignites one target now.
func (e *Engine) Ignite(key string) error { return e.send("ignite", key) }

// ChannelResult is the outcome of a test notification on one channel.
type ChannelResult struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	// Response is the start of the webhook response.
	Response string `json:"response,omitempty"`
	// Unchecked is true for a webhook without success_json, where only the
	// HTTP status was checked.
	Unchecked bool `json:"unchecked,omitempty"`
}

// TestNotify sends a test notification to each configured channel. A
// webhook whose settings were not applied is reported as failed.
func (e *Engine) TestNotify() ([]ChannelResult, error) {
	reply := e.dispatch("test_notify", "")
	return reply.results, reply.err
}

func (e *Engine) runCommand(ctx context.Context, cmd command) {
	defer func() {
		if r := recover(); r != nil {
			cmd.reply <- commandReply{err: fmt.Errorf("内部错误：%v", r)}
		}
	}()
	cfg := e.config()
	var err error
	var results []ChannelResult
	switch cmd.kind {
	case "refresh":
		e.poll(ctx, cfg, e.now(), cmd.arg, true)
		e.mu.Lock()
		if cmd.arg != "" {
			if text, ok := e.pollErrs[cmd.arg]; ok {
				err = errors.New(text)
			}
		} else if text, ok := e.pollErrs[""]; ok {
			err = errors.New(text)
		}
		e.mu.Unlock()
	case "ignite":
		err = errNoTarget
		for _, t := range e.collectTargets(cfg) {
			if t.group.Key == cmd.arg {
				err = e.igniteTarget(ctx, cfg, t, true)
				break
			}
		}
	case "test_notify":
		results, err = e.testNotify(ctx)
	case "language":
		lang := notify.NormalizeLang(cmd.arg)
		e.mu.Lock()
		e.lang = lang
		e.mu.Unlock()
		e.state.Language = lang
		e.dirty = true
		if e.alerts != nil {
			e.alerts.Lang = lang
		}
	default:
		err = fmt.Errorf("未知操作：%s", cmd.kind)
	}
	e.refreshTargets(cfg)
	e.saveState()
	cmd.reply <- commandReply{err: err, results: results}
}

// testNotify sends the test message to all channels at once, so the test
// takes as long as the slowest channel and stays within commandTimeout.
func (e *Engine) testNotify(ctx context.Context) ([]ChannelResult, error) {
	var results []ChannelResult
	e.mu.Lock()
	notifyErr := e.notifyErr
	e.mu.Unlock()
	if notifyErr != "" {
		results = append(results, ChannelResult{Channel: "Webhook", Error: notifyErr})
	}
	var channels []notify.Channel
	if e.alerts != nil {
		if fanout, ok := e.alerts.Sender.(*notify.Fanout); ok {
			channels = fanout.Channels
		}
	}
	if len(results) == 0 && len(channels) == 0 {
		return nil, notify.ErrNotConfigured
	}
	lang := e.language()
	msg := notify.Message{
		Kind:  notify.KindTest,
		Title: notify.T(lang, "test_title"),
		Body:  notify.T(lang, "test_body"),
		Level: notify.LevelActive,
	}
	tested := make([]ChannelResult, len(channels))
	var wg sync.WaitGroup
	for i, channel := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := ChannelResult{Channel: channel.Name}
			defer func() {
				if r := recover(); r != nil {
					result.OK, result.Error = false, fmt.Sprintf("内部错误：%v", r)
				}
				tested[i] = result
			}()
			var err error
			if webhook, ok := channel.Sender.(*notify.Webhook); ok {
				result.Response, err = webhook.Deliver(ctx, msg)
				result.Unchecked = !webhook.Checked()
			} else {
				err = channel.Sender.Send(ctx, msg)
			}
			result.OK = err == nil
			if err != nil {
				result.Error = err.Error()
			}
		}()
	}
	wg.Wait()
	return append(tested, results...), nil
}

// WindowStatus is one window on the status page.
type WindowStatus struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Short      string    `json:"short"`
	Remaining  float64   `json:"remaining"`
	Reset      time.Time `json:"reset,omitempty"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

// GroupStatus is one quota group on the status page.
type GroupStatus struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	SourceLabel string         `json:"source_label"`
	Windows     []WindowStatus `json:"windows"`
}

// AccountStatus is one credential on the status page.
type AccountStatus struct {
	AuthIndex   string `json:"auth_index"`
	Provider    string `json:"provider"`
	Title       string `json:"title"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name"`
	Unavailable bool   `json:"unavailable"`
	// CooldownUntil is when CPA ends its cooldown of the account.
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	// StaleCooldown is true when the cooldown outlasts a recovered quota.
	StaleCooldown bool          `json:"stale_cooldown,omitempty"`
	Error         string        `json:"error,omitempty"`
	Groups        []GroupStatus `json:"groups"`
}

// Status is the response of the status endpoint.
type Status struct {
	Version     string `json:"version"`
	Running     bool   `json:"running"`
	Enabled     bool   `json:"enabled"`
	ConfigError string `json:"config_error,omitempty"`
	LockError   string `json:"lock_error,omitempty"`
	ModelsError string `json:"models_error,omitempty"`
	ListError   string `json:"list_error,omitempty"`
	// NotifyError is the reason the webhook settings were not applied.
	NotifyError string `json:"notify_error,omitempty"`
	DataDir     string `json:"data_dir"`
	// Language is the notification language, "zh" or "en".
	Language string `json:"language"`
	// Timezone and UTCOffset describe the effective display time zone.
	Timezone  string          `json:"timezone"`
	UTCOffset int             `json:"utc_offset_seconds"`
	Now       time.Time       `json:"now"`
	LastPoll  time.Time       `json:"last_poll,omitempty"`
	NextPoll  time.Time       `json:"next_poll,omitempty"`
	Config    config.Config   `json:"config"`
	Accounts  []AccountStatus `json:"accounts"`
	Targets   []TargetView    `json:"targets"`
	Events    []Event         `json:"events"`
	// NextModels is the model automatic selection picks next, per provider.
	NextModels map[string]string `json:"next_models,omitempty"`
}

// Status returns a snapshot for the management page. Secrets are removed
// from the config: only whether they are set is reported.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := e.cfg
	if cfg.BarkURL != "" {
		cfg.BarkURL = "已设置"
	}
	if cfg.ModelsAPIKey != "" {
		cfg.ModelsAPIKey = "已设置"
	}
	// The webhook URL, headers and body can hold tokens and topic names.
	if cfg.Webhook.URL != "" {
		cfg.Webhook.URL = "已设置"
	}
	if cfg.Webhook.Body != "" {
		cfg.Webhook.Body = "已设置"
	}
	if len(cfg.Webhook.Headers) > 0 {
		headers := make(map[string]string, len(cfg.Webhook.Headers))
		for name := range cfg.Webhook.Headers {
			headers[name] = "已设置"
		}
		cfg.Webhook.Headers = headers
	}
	status := Status{
		Version:     e.version,
		Running:     e.running,
		Enabled:     e.enabled,
		ConfigError: e.cfgErr,
		LockError:   e.lockErr,
		ModelsError: e.modelsErr,
		NextModels:  maps.Clone(e.nextModels),
		ListError:   e.pollErrs[""],
		NotifyError: e.notifyErr,
		Language:    notify.NormalizeLang(e.lang),
		DataDir:     e.dataDir,
		Timezone:    e.cfg.Location().String(),
		Now:         e.now().UTC(),
		LastPoll:    e.lastPoll,
		NextPoll:    e.nextPoll,
		Config:      cfg,
		Targets:     append([]TargetView{}, e.targets...),
	}
	_, status.UTCOffset = e.now().In(e.cfg.Location()).Zone()
	creds := make([]*Cred, 0, len(e.creds))
	for _, cred := range e.creds {
		creds = append(creds, cred)
	}
	sort.SliceStable(creds, func(i, j int) bool {
		if creds[i].Provider != creds[j].Provider {
			return providerRank(creds[i].Provider) < providerRank(creds[j].Provider)
		}
		return creds[i].Suffix < creds[j].Suffix
	})
	for _, cred := range creds {
		account := AccountStatus{
			AuthIndex:   cred.AuthIndex,
			Provider:    cred.Provider,
			Title:       labelFor(quota.ProviderTitle(cred.Provider), cred),
			Email:       cred.Email,
			Name:        cred.Name,
			Unavailable: cred.Unavailable,
			Error:       e.pollErrs[cred.AuthIndex],
		}
		var groups []*GroupView
		for _, group := range e.groups {
			if group.AuthIndex == cred.AuthIndex {
				groups = append(groups, group)
			}
		}
		sort.SliceStable(groups, func(i, j int) bool {
			if ri, rj := groupRank(groups[i].SourceLabel), groupRank(groups[j].SourceLabel); ri != rj {
				return ri < rj
			}
			return groups[i].Key < groups[j].Key
		})
		if now := e.now(); cred.CooldownUntil.After(now) {
			account.CooldownUntil = cred.CooldownUntil
			account.StaleCooldown = staleCooldown(cred, groups, now)
		}
		for _, group := range groups {
			gs := GroupStatus{Key: group.Key, Label: group.Label, SourceLabel: group.SourceLabel}
			for _, w := range group.Windows {
				gs.Windows = append(gs.Windows, WindowStatus{
					ID:         w.ID,
					Label:      w.Label,
					Short:      quota.ShortLabel(w.Label),
					Remaining:  w.Remaining,
					Reset:      w.Reset,
					Source:     string(w.Source),
					ObservedAt: w.ObservedAt,
				})
			}
			account.Groups = append(account.Groups, gs)
		}
		status.Accounts = append(status.Accounts, account)
	}
	for i := len(e.events) - 1; i >= 0 && len(status.Events) < 100; i-- {
		status.Events = append(status.Events, e.events[i])
	}
	if status.Accounts == nil {
		status.Accounts = []AccountStatus{}
	}
	if status.Events == nil {
		status.Events = []Event{}
	}
	return status
}

// groupOrder lists quota groups in the order the page shows them, the same
// as SERVICE_ORDER in internal/web/page.js. Other groups follow by key.
var groupOrder = []string{"Claude", "ChatGPT", "Gemini", "Fable", "Claude / GPT"}

func groupRank(sourceLabel string) int {
	for i, label := range groupOrder {
		if label == sourceLabel {
			return i
		}
	}
	return len(groupOrder)
}

func providerRank(provider string) int {
	for i, name := range config.SupportedProviders {
		if name == provider {
			return i
		}
	}
	return len(config.SupportedProviders)
}

// Series is the history of one window.
type Series struct {
	Group       string       `json:"group"`
	Window      string       `json:"window"`
	WindowLabel string       `json:"window_label"`
	Label       string       `json:"label"`
	SourceLabel string       `json:"source_label"` // label without the account suffix
	Provider    string       `json:"provider"`
	AuthIndex   string       `json:"auth_index"`
	Points      [][3]float64 `json:"points"` // [unix seconds, remaining, 1 for passive]
}

// HistoryResponse is the response of the history endpoint.
type HistoryResponse struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Bucket int64     `json:"bucket_seconds"`
	Series []Series  `json:"series"`
	Events []Event   `json:"events"`
}

// historyRanges are the fixed spans the chart offers, in hours for the
// 5-hour window and days for the 7-day window: one unit, half of a window
// plus one, a window plus one, half a day or month (two whole windows), a
// day or a month, and five windows plus one. See internal/web/page.js.
var historyRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"24h": 24 * time.Hour,
	"26h": 26 * time.Hour,
	"4d":  4 * 24 * time.Hour,
	"8d":  8 * 24 * time.Hour,
	"15d": 15 * 24 * time.Hour,
	"36d": 36 * 24 * time.Hour,
}

// RangeSpan returns the span of a history range, 24 hours for an unknown
// one. "1mo" reaches back to the same date of the previous month in loc, or
// to its last day when that month is shorter, so it covers one monthly
// billing period.
func RangeSpan(name string, now time.Time, loc *time.Location) time.Duration {
	if span, ok := historyRanges[name]; ok {
		return span
	}
	if name == "1mo" {
		local := now.In(loc)
		year, month, day := local.Date()
		if last := time.Date(year, month, 0, 0, 0, 0, 0, loc).Day(); day > last {
			day = last
		}
		from := time.Date(year, month-1, day, local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), loc)
		return now.Sub(from)
	}
	return 24 * time.Hour
}

// HistoryRange returns the history for a named range, see RangeSpan.
func (e *Engine) HistoryRange(name string) (HistoryResponse, error) {
	return e.History(RangeSpan(name, e.now(), e.config().Location()))
}

// History returns quota samples and events for the last span. Long spans are
// reduced to the last sample per bucket.
func (e *Engine) History(span time.Duration) (HistoryResponse, error) {
	e.mu.Lock()
	dataDir := e.dataDir
	labels := map[string]GroupView{}
	for key, group := range e.groups {
		labels[key] = GroupView{Key: key, Provider: group.Provider, AuthIndex: group.AuthIndex, SourceLabel: group.SourceLabel, Label: group.Label, Windows: group.Windows}
	}
	e.mu.Unlock()
	now := e.now().UTC()
	resp := HistoryResponse{From: now.Add(-span), To: now, Series: []Series{}, Events: []Event{}}
	if dataDir == "" {
		return resp, nil
	}
	switch {
	case span > 8*24*time.Hour:
		resp.Bucket = 1800
	case span > 2*24*time.Hour:
		resp.Bucket = 300
	}
	history := &store.History{Dir: dataDir + "/history"}
	records, err := history.Query(resp.From, now)
	if err != nil {
		return resp, err
	}
	series := map[string]*Series{}
	var order []string
	for _, record := range records {
		if record.Kind == "e" {
			if record.Event == "ignite" || record.Event == "ignite_manual" || record.Event == "ignite_failed" || record.Event == "ignite_paused" {
				resp.Events = append(resp.Events, Event{
					Time: time.Unix(record.Time, 0).UTC(), Level: record.Level, Kind: record.Event,
					Group: record.Group, Message: record.Message, Label: record.Label, Detail: record.Detail, Params: record.Params,
				})
			}
			continue
		}
		key := record.Group + "|" + record.Window
		s, ok := series[key]
		if !ok {
			s = &Series{Group: record.Group, Window: record.Window, WindowLabel: record.Window}
			if group, ok := labels[record.Group]; ok {
				s.Label = group.Label
				s.SourceLabel = group.SourceLabel
				s.Provider = group.Provider
				s.AuthIndex = group.AuthIndex
				for _, w := range group.Windows {
					if w.ID == record.Window {
						s.WindowLabel = quota.ShortLabel(w.Label)
					}
				}
			} else {
				s.Label = record.Group
				s.SourceLabel = record.Group
			}
			series[key] = s
			order = append(order, key)
		}
		passive := 0.0
		if record.Source == string(quota.SourcePassive) {
			passive = 1
		}
		point := [3]float64{float64(record.Time), record.Remaining, passive}
		if resp.Bucket > 0 && len(s.Points) > 0 {
			last := s.Points[len(s.Points)-1]
			if int64(last[0])/resp.Bucket == record.Time/resp.Bucket {
				s.Points[len(s.Points)-1] = point
				continue
			}
		}
		s.Points = append(s.Points, point)
	}
	for _, key := range order {
		resp.Series = append(resp.Series, *series[key])
	}
	return resp, nil
}

// notified records a delivered notification. The page shows its title, which
// is already in the notification language.
func (e *Engine) notified(group string, msg notify.Message) {
	e.addEventDetail("info", "notify", group, msg.Label, msg.Title, msg.Title, map[string]string{"title": msg.Title})
}

// notifyFailed records a notification that a channel did not accept.
func (e *Engine) notifyFailed(group string, msg notify.Message, err error) {
	e.addEventDetail("warn", "notify_failed", group, msg.Label, fmt.Sprintf("%s：%v", msg.Title, err),
		fmt.Sprintf("推送失败 %s：%v", msg.Title, err), map[string]string{"title": msg.Title, "error": err.Error()})
}

// language returns the notification language.
func (e *Engine) language() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return notify.NormalizeLang(e.lang)
}

// SetLanguage records the Management Center language reported by the page;
// notifications use it from then on.
func (e *Engine) SetLanguage(lang string) error { return e.send("language", lang) }

// eventTime formats a time for event params; the page shows it in the
// plugin's time zone.
func eventTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
