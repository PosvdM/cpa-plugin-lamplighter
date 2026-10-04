package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
}

func (e *Engine) addEvent(level, kind, group, message string) {
	e.addEventDetail(level, kind, group, "", message, message)
}

// addEventDetail records an event. message is the full text for the CPA log;
// label and detail are the quota group and a short description for the page,
// which shows the event type separately.
func (e *Engine) addEventDetail(level, kind, group, label, detail, message string) {
	now := e.now()
	e.mu.Lock()
	e.events = append(e.events, Event{Time: now, Level: level, Kind: kind, Group: group, Message: message, Label: label, Detail: detail})
	if len(e.events) > maxEvents {
		e.events = e.events[len(e.events)-maxEvents:]
	}
	e.mu.Unlock()
	if e.history != nil {
		e.history.Append(store.Record{Kind: "e", Time: now.Unix(), Group: group, Event: kind, Level: level, Message: message, Label: label, Detail: detail})
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
	reply chan error
}

// ErrNotRunning means the background loop has not started yet.
var ErrNotRunning = errors.New("Lamplighter 尚未就绪：CPA 启动后约 20 秒开始运行，或插件已在配置中停用")

func (e *Engine) send(kind, arg string) error {
	e.mu.Lock()
	ready := e.running && e.enabled
	e.mu.Unlock()
	if !ready {
		return ErrNotRunning
	}
	cmd := command{kind: kind, arg: arg, reply: make(chan error, 1)}
	timer := time.NewTimer(commandTimeout)
	defer timer.Stop()
	select {
	case e.cmdCh <- cmd:
	case <-timer.C:
		return errors.New("后台任务繁忙，请稍后再试")
	case <-e.stopCh:
		return ErrNotRunning
	}
	select {
	case err := <-cmd.reply:
		return err
	case <-timer.C:
		return errors.New("操作超时")
	case <-e.stopCh:
		return ErrNotRunning
	}
}

// Refresh runs an active query now, for one credential or for all when
// authIndex is empty.
func (e *Engine) Refresh(authIndex string) error { return e.send("refresh", authIndex) }

// Ignite ignites one target now.
func (e *Engine) Ignite(key string) error { return e.send("ignite", key) }

// TestBark sends a test notification.
func (e *Engine) TestBark() error { return e.send("test_bark", "") }

func (e *Engine) runCommand(ctx context.Context, cmd command) {
	defer func() {
		if r := recover(); r != nil {
			cmd.reply <- fmt.Errorf("内部错误：%v", r)
		}
	}()
	cfg := e.config()
	var err error
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
	case "test_bark":
		if e.alerts == nil || e.alerts.Sender == nil {
			err = notify.ErrNotConfigured
			break
		}
		err = e.alerts.Sender.Send(ctx, notify.Message{
			Title: "🕯️ Lamplighter 测试通知",
			Body:  "Bark 推送配置正常。",
			Level: notify.LevelActive,
		})
	default:
		err = fmt.Errorf("未知操作：%s", cmd.kind)
	}
	e.refreshTargets(cfg)
	e.saveState()
	cmd.reply <- err
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
	AuthIndex   string        `json:"auth_index"`
	Provider    string        `json:"provider"`
	Title       string        `json:"title"`
	Email       string        `json:"email,omitempty"`
	Name        string        `json:"name"`
	Unavailable bool          `json:"unavailable"`
	Error       string        `json:"error,omitempty"`
	Groups      []GroupStatus `json:"groups"`
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
	DataDir     string `json:"data_dir"`
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
	status := Status{
		Version:     e.version,
		Running:     e.running,
		Enabled:     e.enabled,
		ConfigError: e.cfgErr,
		LockError:   e.lockErr,
		ModelsError: e.modelsErr,
		ListError:   e.pollErrs[""],
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
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })
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

// History returns quota samples and events for the last span. Long spans are
// reduced to the last sample per bucket.
func (e *Engine) History(span time.Duration) (HistoryResponse, error) {
	e.mu.Lock()
	dataDir := e.dataDir
	labels := map[string]GroupView{}
	for key, group := range e.groups {
		labels[key] = GroupView{Key: key, Provider: group.Provider, AuthIndex: group.AuthIndex, Label: group.Label, Windows: group.Windows}
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
					Group: record.Group, Message: record.Message, Label: record.Label, Detail: record.Detail,
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
				s.Provider = group.Provider
				s.AuthIndex = group.AuthIndex
				for _, w := range group.Windows {
					if w.ID == record.Window {
						s.WindowLabel = quota.ShortLabel(w.Label)
					}
				}
			} else {
				s.Label = record.Group
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
