// Package store persists Lamplighter state and quota history in the plugin
// data directory.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// WindowState is the notification baseline of one quota window.
type WindowState struct {
	Severity                string  `json:"severity,omitempty"`
	NotifiedSeverity        string  `json:"notified_severity,omitempty"`
	Remaining               float64 `json:"remaining"`
	Reset                   string  `json:"reset,omitempty"`
	LastSeen                int64   `json:"last_seen,omitempty"`
	ResetNotice1hFor        string  `json:"reset_notice_1h_for,omitempty"`
	ResetNotice1dFor        string  `json:"reset_notice_1d_for,omitempty"`
	ResetRecoveryFor        string  `json:"reset_recovery_for,omitempty"`
	PendingResetRecoveryFor string  `json:"pending_reset_recovery_for,omitempty"`
}

// GroupState holds the notification baselines of one quota group.
type GroupState struct {
	Label    string                  `json:"label,omitempty"`
	LastSeen int64                   `json:"last_seen,omitempty"`
	Windows  map[string]*WindowState `json:"windows"`
}

// Observation is the last observed 5-hour reset of an ignition target.
type Observation struct {
	SeenEpoch  float64  `json:"seen_epoch"`
	ResetEpoch *float64 `json:"reset_epoch,omitempty"`
}

// TargetState is the ignition schedule and failure protection state of one
// ignition target.
type TargetState struct {
	RollingReset              bool         `json:"rolling_reset,omitempty"`
	ResetObservation          *Observation `json:"reset_observation,omitempty"`
	LastSuccessEpoch          float64      `json:"last_success_epoch,omitempty"`
	LastAttemptEpoch          float64      `json:"last_attempt_epoch,omitempty"`
	LastModel                 string       `json:"last_model,omitempty"`
	LastError                 string       `json:"last_error,omitempty"`
	LastFailureEpoch          float64      `json:"last_failure_epoch,omitempty"`
	ConsecutiveFailures       int          `json:"consecutive_failures,omitempty"`
	RetryAtEpoch              float64      `json:"retry_at_epoch,omitempty"`
	CircuitOpenUntilEpoch     float64      `json:"circuit_open_until_epoch,omitempty"`
	CircuitReason             string       `json:"circuit_reason,omitempty"`
	CircuitNotifiedUntilEpoch float64      `json:"circuit_notified_until_epoch,omitempty"`
}

// SentEvent is the last notification sent for one scheduled reset.
type SentEvent struct {
	Key string `json:"key"`
	// AnnouncedAt is the announcedAt of the sent post, in Unix seconds.
	AnnouncedAt int64 `json:"announced_at,omitempty"`
	// Content is the notification body in UTC and English, used to tell
	// whether a later post changes anything.
	Content string `json:"content"`
}

// CodexResetState deduplicates Did Codex Reset records.
type CodexResetState struct {
	Initialized bool     `json:"initialized,omitempty"`
	SeenKeys    []string `json:"seen_keys,omitempty"`
	// SentEvents lists the scheduled resets already notified, newest first.
	SentEvents     []SentEvent `json:"sent_events,omitempty"`
	LastCheckEpoch int64       `json:"last_check_epoch,omitempty"`
}

// State is the content of state.json.
type State struct {
	Version    int                     `json:"version"`
	Groups     map[string]*GroupState  `json:"groups"`
	Scheduler  map[string]*TargetState `json:"scheduler"`
	CodexReset *CodexResetState        `json:"codex_reset_updates,omitempty"`
	// CooldownNotices maps an auth index to the end of the stale CPA
	// cooldown that was already notified, in Unix seconds.
	CooldownNotices map[string]int64 `json:"cooldown_notices,omitempty"`
	// Language is the notification language, the Management Center language
	// last reported by the management page.
	Language string `json:"language,omitempty"`
}

// CooldownNotice records that the stale cooldown of authIndex ending at
// until was notified.
func (s *State) CooldownNotice(authIndex string, until int64) {
	if s.CooldownNotices == nil {
		s.CooldownNotices = map[string]int64{}
	}
	s.CooldownNotices[authIndex] = until
}

// NewState returns an empty state.
func NewState() *State {
	return &State{
		Version:   1,
		Groups:    map[string]*GroupState{},
		Scheduler: map[string]*TargetState{},
	}
}

func (s *State) ensure() {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Groups == nil {
		s.Groups = map[string]*GroupState{}
	}
	if s.Scheduler == nil {
		s.Scheduler = map[string]*TargetState{}
	}
	for key, group := range s.Groups {
		if group == nil {
			s.Groups[key] = &GroupState{Windows: map[string]*WindowState{}}
		} else if group.Windows == nil {
			group.Windows = map[string]*WindowState{}
		}
	}
}

// Group returns the state of group key, creating it when missing.
func (s *State) Group(key string) *GroupState {
	s.ensure()
	group, ok := s.Groups[key]
	if !ok {
		group = &GroupState{Windows: map[string]*WindowState{}}
		s.Groups[key] = group
	}
	return group
}

// Target returns the state of ignition target id, creating it when missing.
func (s *State) Target(id string) *TargetState {
	s.ensure()
	target, ok := s.Scheduler[id]
	if !ok || target == nil {
		target = &TargetState{}
		s.Scheduler[id] = target
	}
	return target
}

// StateFile reads and writes state.json. Writes go to a temporary file that
// replaces the old file, so a crash never leaves a partly written state.
type StateFile struct {
	Path string
	mu   sync.Mutex
}

// Load reads the state. A missing file yields an empty state and fresh=true.
func (f *StateFile) Load() (state *State, fresh bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewState(), true, nil
	}
	if err != nil {
		return NewState(), true, err
	}
	state = NewState()
	if err := json.Unmarshal(raw, state); err != nil {
		return NewState(), true, fmt.Errorf("状态文件损坏，已重建: %w", err)
	}
	state.ensure()
	return state, false, nil
}

// Save writes state atomically.
func (f *StateFile) Save(state *State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}
