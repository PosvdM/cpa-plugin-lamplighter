// Package engine runs Lamplighter's background loop: quota polling, passive
// quota updates, ignition, notifications and history.
//
// One goroutine owns the state and performs all network work in order.
// usage.handle and the management API talk to it through channels, so the
// host callbacks never block on upstream requests.
package engine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/egress"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/ignite"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

const (
	// startupDelay gives CPA time to load credentials and executors.
	startupDelay       = 20 * time.Second
	enabledCheckPeriod = 30 * time.Second
	lockRetryPeriod    = 10 * time.Second
	pruneInterval      = 6 * time.Hour
	maxEvents          = 200
	maxSleep           = time.Hour
	restartDelay       = 30 * time.Second
)

// confirmDelays are the waits between ignition confirmation checks.
var confirmDelays = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second, 15 * time.Second}

// Options configures an Engine.
type Options struct {
	Host           host.Host
	Version        string
	DataDir        string
	HostConfigPath string
	// Now and Sleep can be replaced in tests.
	Now func() time.Time
}

// Engine is the Lamplighter background service.
type Engine struct {
	host           host.Host
	version        string
	defaultDataDir string
	hostConfigPath string
	now            func() time.Time

	// mu guards the fields read by the management API and usage.handle.
	mu        sync.Mutex
	cfg       config.Config
	cfgErr    string
	dataDir   string
	enabled   bool
	running   bool
	lockErr   string
	creds     map[string]*Cred
	groups    map[string]*GroupView
	targets   []TargetView
	events    []Event
	lastPoll  time.Time
	nextPoll  time.Time
	pollErrs  map[string]string
	modelsErr string
	lang      string // notification language, a copy of state.Language

	// Owned by the loop goroutine.
	state       *store.State
	stateFile   *store.StateFile
	history     *store.History
	dirty       bool
	egress      *egress.Egress
	lister      *models.Lister
	alerts      *notify.Alerts
	passiveAt   map[string]time.Time
	activeAt    map[string]time.Time
	lastSample  map[string]sampleMark
	pendingSamp map[string]store.Record
	lock        *store.Lock

	usageCh chan usageEvent
	cmdCh   chan command
	wakeCh  chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}
	stopped bool
	started bool
}

// New creates an engine. Start runs it.
func New(opts Options) *Engine {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cfg := config.Default()
	e := &Engine{
		host:           opts.Host,
		version:        opts.Version,
		defaultDataDir: opts.DataDir,
		hostConfigPath: opts.HostConfigPath,
		now:            now,
		cfg:            cfg,
		enabled:        true,
		creds:          map[string]*Cred{},
		groups:         map[string]*GroupView{},
		pollErrs:       map[string]string{},
		passiveAt:      map[string]time.Time{},
		activeAt:       map[string]time.Time{},
		lastSample:     map[string]sampleMark{},
		pendingSamp:    map[string]store.Record{},
		lister:         &models.Lister{},
		usageCh:        make(chan usageEvent, 512),
		cmdCh:          make(chan command),
		wakeCh:         make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
	e.egress = &egress.Egress{Host: opts.Host, Timeout: cfg.RequestTimeout()}
	return e
}

// Configure applies a new configuration. An invalid configuration keeps the
// previous one and is reported on the status page.
func (e *Engine) Configure(cfg config.Config, parseErr error) {
	e.mu.Lock()
	if parseErr != nil {
		e.cfgErr = parseErr.Error()
		e.mu.Unlock()
		return
	}
	e.cfg = cfg
	e.cfgErr = ""
	e.mu.Unlock()
	e.wake()
}

func (e *Engine) config() config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

func (e *Engine) wake() {
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}

// Start launches the background loop once.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.started || e.stopped {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()
	go e.supervise()
}

// Stop ends the loop and waits up to timeout for it to finish. A stopped
// engine never starts again; CPA loads a new instance instead.
func (e *Engine) Stop(timeout time.Duration) {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	started := e.started
	close(e.stopCh)
	e.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-e.doneCh:
	case <-time.After(timeout):
	}
}

func (e *Engine) stopping() bool {
	select {
	case <-e.stopCh:
		return true
	default:
		return false
	}
}

// sleep waits for d or until the engine stops. It returns false when stopping.
func (e *Engine) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-e.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// supervise restarts the loop after a panic. A panic inside the plugin would
// otherwise terminate the whole CPA process.
func (e *Engine) supervise() {
	defer close(e.doneCh)
	defer e.releaseLock()
	for !e.stopping() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					e.logf("error", "后台循环异常，将在 30 秒后重启：%v\n%s", r, debug.Stack())
				}
			}()
			e.run()
		}()
		if !e.sleep(restartDelay) {
			return
		}
	}
}

func (e *Engine) releaseLock() {
	if e.lock != nil {
		e.lock.Release()
		e.lock = nil
	}
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
}

func (e *Engine) logf(level, format string, args ...any) {
	if e.host == nil {
		return
	}
	defer func() { _ = recover() }()
	e.host.Log(level, fmt.Sprintf(format, args...), nil)
}

func (e *Engine) resolveDataDir(cfg config.Config) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	if e.defaultDataDir != "" {
		return e.defaultDataDir
	}
	return filepath.Join("plugins", "data", config.PluginID)
}

// checkEnabled reads the CPA config file. When the file cannot be read, the
// plugin assumes it is enabled.
func (e *Engine) checkEnabled() bool {
	enabled := true
	if e.hostConfigPath != "" {
		if raw, err := os.ReadFile(e.hostConfigPath); err == nil {
			if value, err := config.PluginEnabled(raw); err == nil {
				enabled = value
			}
		}
	}
	e.mu.Lock()
	changed := e.enabled != enabled
	e.enabled = enabled
	e.mu.Unlock()
	if changed {
		if enabled {
			e.logf("info", "插件已重新启用")
		} else {
			e.logf("info", "插件已在 CPA 配置中停用，暂停所有后台任务")
		}
	}
	return enabled
}

func (e *Engine) openStores(cfg config.Config) error {
	dir := e.resolveDataDir(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	e.stateFile = &store.StateFile{Path: filepath.Join(dir, "state.json")}
	state, _, err := e.stateFile.Load()
	if err != nil {
		e.logf("warn", "%v", err)
	}
	e.state = state
	e.history = &store.History{Dir: filepath.Join(dir, "history")}
	e.mu.Lock()
	e.dataDir = dir
	e.lang = notify.NormalizeLang(state.Language)
	e.mu.Unlock()
	if e.alerts != nil {
		e.alerts.Lang = e.language()
	}
	e.loadRecentEvents()
	return nil
}

func (e *Engine) acquireLock(dir string) bool {
	for !e.stopping() {
		lock, ok, err := store.TryLock(filepath.Join(dir, "instance.lock"))
		if err != nil {
			e.mu.Lock()
			e.lockErr = err.Error()
			e.mu.Unlock()
			return false
		}
		if ok {
			e.lock = lock
			e.mu.Lock()
			e.lockErr = ""
			e.mu.Unlock()
			return true
		}
		e.mu.Lock()
		e.lockErr = "另一个 Lamplighter 实例仍在运行，等待它退出"
		e.mu.Unlock()
		if !e.sleep(lockRetryPeriod) {
			return false
		}
	}
	return false
}

func (e *Engine) saveState() {
	if !e.dirty || e.stateFile == nil {
		return
	}
	if err := e.stateFile.Save(e.state); err != nil {
		e.logf("warn", "保存状态失败：%v", err)
		return
	}
	e.dirty = false
}

func (e *Engine) applyRuntimeConfig(cfg config.Config) {
	e.egress.Timeout = cfg.RequestTimeout()
	e.alerts = &notify.Alerts{
		Cfg:  cfg,
		Lang: e.language(),
		Sender: &notify.Bark{
			URL:       cfg.BarkURL,
			Group:     cfg.BarkGroup,
			Icon:      cfg.BarkIcon,
			UserAgent: "cpa-plugin-lamplighter/" + e.version,
			Client:    &http.Client{Timeout: cfg.RequestTimeout()},
		},
		OnSent:  func(msg notify.Message) { e.notified("", msg) },
		OnError: func(msg notify.Message, err error) { e.notifyFailed("", msg, err) },
	}
}

func alignedAfter(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return t.Truncate(interval).Add(interval)
}

func (e *Engine) run() {
	if !e.sleep(startupDelay) {
		return
	}
	cfg := e.config()
	if err := e.openStores(cfg); err != nil {
		e.logf("error", "%v", err)
		e.addEvent("error", "error", "", err.Error())
		return
	}
	if !e.acquireLock(e.dataDir) {
		return
	}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	e.applyRuntimeConfig(cfg)
	e.logf("info", "Lamplighter %s 已启动，数据目录 %s", e.version, e.dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-e.stopCh
		cancel()
	}()

	now := e.now()
	nextPoll := now
	nextFeed := now
	nextEnabledCheck := now
	nextPrune := now
	appliedCfg := cfg
	for !e.stopping() {
		now = e.now()
		cfg = e.config()
		if !sameRuntimeConfig(cfg, appliedCfg) {
			e.applyRuntimeConfig(cfg)
			if cfg.PollIntervalSeconds != appliedCfg.PollIntervalSeconds {
				nextPoll = now
			}
			appliedCfg = cfg
		}
		if !now.Before(nextEnabledCheck) {
			e.checkEnabled()
			nextEnabledCheck = now.Add(enabledCheckPeriod)
		}
		e.mu.Lock()
		enabled := e.enabled
		e.mu.Unlock()

		if enabled {
			e.drainUsage(ctx)
			if cfg.CodexResetUpdates.Enabled && !now.Before(nextFeed) {
				e.pollResetFeed(ctx, cfg)
				nextFeed = alignedAfter(e.now(), time.Duration(cfg.CodexResetUpdates.PollSeconds)*time.Second)
			}
			if !now.Before(nextPoll) {
				e.poll(ctx, cfg, nextPoll, "", false)
				nextPoll = alignedAfter(e.now(), cfg.PollInterval())
			}
			if cfg.Ignition.Enabled {
				e.performDue(ctx, cfg)
			}
			if !now.Before(nextPrune) {
				e.history.Prune(e.now(), time.Duration(cfg.HistoryRetentionDays)*24*time.Hour)
				nextPrune = e.now().Add(pruneInterval)
			}
			e.flushSamples(e.now(), false)
			e.refreshTargets(cfg)
			e.saveState()
		}
		e.mu.Lock()
		e.nextPoll = nextPoll
		e.mu.Unlock()

		wakeAt := now.Add(maxSleep)
		for _, candidate := range []time.Time{nextPoll, nextEnabledCheck, nextPrune} {
			if candidate.Before(wakeAt) {
				wakeAt = candidate
			}
		}
		if enabled && cfg.CodexResetUpdates.Enabled && nextFeed.Before(wakeAt) {
			wakeAt = nextFeed
		}
		if enabled && cfg.Ignition.Enabled {
			if due := e.nextDue(cfg); !due.IsZero() && due.Before(wakeAt) {
				wakeAt = due
			}
		}
		if len(e.pendingSamp) > 0 {
			if flush := e.now().Add(time.Minute); flush.Before(wakeAt) {
				wakeAt = flush
			}
		}
		wait := wakeAt.Sub(e.now())
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-e.stopCh:
			timer.Stop()
		case <-e.wakeCh:
			timer.Stop()
		case ev := <-e.usageCh:
			timer.Stop()
			if enabled {
				e.applyUsage(ctx, ev)
			}
		case cmd := <-e.cmdCh:
			timer.Stop()
			e.runCommand(ctx, cmd)
		case <-timer.C:
		}
	}
	e.flushSamples(e.now(), true)
	e.saveState()
}

// sameRuntimeConfig reports whether the settings used to build the Bark
// sender and HTTP clients are unchanged.
func sameRuntimeConfig(a, b config.Config) bool {
	return a.BarkURL == b.BarkURL && a.BarkGroup == b.BarkGroup && a.BarkIcon == b.BarkIcon &&
		a.RequestTimeoutSeconds == b.RequestTimeoutSeconds && a.NoticeThreshold == b.NoticeThreshold &&
		a.LowThreshold == b.LowThreshold && a.CriticalThreshold == b.CriticalThreshold &&
		a.NotifyRecovery == b.NotifyRecovery && a.NotifyResetReminders == b.NotifyResetReminders &&
		a.Location().String() == b.Location().String() && a.PollIntervalSeconds == b.PollIntervalSeconds
}

// waitWithUsage sleeps for d while applying passive updates as they arrive.
func (e *Engine) waitWithUsage(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-e.stopCh:
			return false
		case ev := <-e.usageCh:
			e.applyUsage(ctx, ev)
		case <-timer.C:
			return true
		}
	}
}

func (e *Engine) schedule(cfg config.Config) ignite.Schedule {
	return ignite.Schedule{Cfg: cfg.Ignition, Loc: cfg.Location()}
}

// labelFor returns the display label of a group for cred.
func labelFor(sourceLabel string, cred *Cred) string {
	if cred == nil || cred.Suffix == "" {
		return sourceLabel
	}
	return sourceLabel + cred.Suffix
}

// providerLabel returns the source label used for a provider's group.
func providerLabel(provider, groupLabel string) string {
	if provider == "codex" {
		return quota.ProviderTitle(provider)
	}
	return groupLabel
}
