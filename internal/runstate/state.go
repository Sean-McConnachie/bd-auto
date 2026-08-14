// Package runstate owns .beads/auto/run.json, the durable state of a bd-auto
// run.
//
// This file is the reason a run survives autocompact: everything the
// orchestrator needs to resume is here, not in a context window. It is also
// written concurrently by hooks firing inside parallel workers, so every
// mutation goes through Update, which holds an exclusive flock across the whole
// read-modify-write. The concurrency spike (eqc.1) showed that an unlocked
// read-modify-write loses writes silently, with a zero exit code; that failure
// mode is not repeated here.
package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Schema version for run.json. Bump when the shape changes incompatibly.
const Version = 1

// Status values for a run.
const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusDone   = "done"
)

// ErrNoRun is returned when no run is active.
var ErrNoRun = errors.New("no active run")

// Attempt records one worker's pass at an issue.
type Attempt struct {
	AgentID   string    `json:"agent_id,omitempty"`
	Branch    string    `json:"branch"`
	Attempt   int       `json:"attempt"`
	StartedAt time.Time `json:"started_at"`
	Stage     string    `json:"stage,omitempty"`
	Rounds    int       `json:"rounds,omitempty"`
}

// Parked is an issue that failed its attempts and was set aside.
type Parked struct {
	ID       string    `json:"id"`
	Reason   string    `json:"reason"`
	Attempts int       `json:"attempts"`
	Stage    string    `json:"stage,omitempty"`
	ParkedAt time.Time `json:"parked_at"`
}

// State is the whole run. Every field is safe to read after Load.
type State struct {
	Version   int       `json:"version"`
	Epic      string    `json:"epic"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Status    string    `json:"status"`

	// Resolved config snapshot, so a run keeps its settings even if
	// .beads-auto.yaml changes underneath it.
	Concurrency int    `json:"concurrency"`
	Autonomy    string `json:"autonomy"`
	Retry       int    `json:"retry"`

	Wave       int      `json:"wave"`
	WaveIssues []string `json:"wave_issues"`

	// InFlight maps issue ID to the attempt currently working it.
	InFlight map[string]Attempt `json:"in_flight"`
	// Bindings maps a subagent's agent_id to the issue it claimed. Written by
	// the PreToolUse hook, read by SubagentStop.
	Bindings map[string]string `json:"bindings"`
	// Attempts counts total attempts per issue across the run.
	Attempts map[string]int `json:"attempts"`

	Done   []string `json:"done"`
	Parked []Parked `json:"parked"`

	// Continuations counts consecutive Stop-hook refusals. It is the runaway
	// guard: if the wave never advances, the run stops itself.
	Continuations  int `json:"continuations"`
	LastWaveChange int `json:"last_wave_change"`

	// Notes is a short human-readable breadcrumb trail, capped.
	Notes []string `json:"notes,omitempty"`
}

const maxNotes = 40

// Dir returns the run-state directory for a repo.
func Dir(repoRoot string) string { return filepath.Join(repoRoot, ".beads", "auto") }

// Path returns the run-state file for a repo.
func Path(repoRoot string) string { return filepath.Join(Dir(repoRoot), "run.json") }

func lockPath(repoRoot string) string { return filepath.Join(Dir(repoRoot), "run.lock") }

// Active reports whether a run is in progress. Hooks call this first and no-op
// when it is false, which is what keeps ordinary sessions unaffected.
func Active(repoRoot string) bool {
	st, err := Load(repoRoot)
	if err != nil {
		return false
	}
	return st.Status == StatusActive || st.Status == StatusPaused
}

// Load reads the run state without locking. Use it for read-only paths; use
// Update for anything that writes.
func Load(repoRoot string) (*State, error) {
	raw, err := os.ReadFile(Path(repoRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoRun
		}
		return nil, err
	}
	st := &State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("parse run state: %w", err)
	}
	st.normalise()
	return st, nil
}

func (s *State) normalise() {
	if s.InFlight == nil {
		s.InFlight = map[string]Attempt{}
	}
	if s.Bindings == nil {
		s.Bindings = map[string]string{}
	}
	if s.Attempts == nil {
		s.Attempts = map[string]int{}
	}
}

// New builds a fresh run state.
func New(epic string, concurrency int, autonomy string, retry int) *State {
	now := time.Now().UTC()
	s := &State{
		Version:     Version,
		Epic:        epic,
		StartedAt:   now,
		UpdatedAt:   now,
		Status:      StatusActive,
		Concurrency: concurrency,
		Autonomy:    autonomy,
		Retry:       retry,
		Wave:        0,
	}
	s.normalise()
	return s
}

// withLock runs fn while holding an exclusive lock on the run directory.
func withLock(repoRoot string, fn func() error) error {
	if err := os.MkdirAll(Dir(repoRoot), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(lockPath(repoRoot), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock run state: %w", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return fn()
}

// Update applies fn to the run state under an exclusive lock and writes the
// result atomically. fn may be called on a fresh state if none exists and
// create is true.
func Update(repoRoot string, create bool, fn func(*State) error) (*State, error) {
	var out *State
	err := withLock(repoRoot, func() error {
		st, err := Load(repoRoot)
		if err != nil {
			if !errors.Is(err, ErrNoRun) || !create {
				return err
			}
			st = &State{Version: Version}
			st.normalise()
		}
		if err := fn(st); err != nil {
			return err
		}
		st.UpdatedAt = time.Now().UTC()
		out = st
		return write(repoRoot, st)
	})
	return out, err
}

// write serialises the state to a temp file and renames it into place, so a
// crash mid-write cannot leave a truncated run.json.
func write(repoRoot string, st *State) error {
	if err := os.MkdirAll(Dir(repoRoot), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := Path(repoRoot) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(repoRoot))
}

// Save writes a state unconditionally, under lock.
func Save(repoRoot string, st *State) error {
	return withLock(repoRoot, func() error {
		st.UpdatedAt = time.Now().UTC()
		return write(repoRoot, st)
	})
}

// Clear removes the run state, disarming every hook.
func Clear(repoRoot string) error {
	return withLock(repoRoot, func() error {
		err := os.Remove(Path(repoRoot))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}

// --- state helpers, all called from inside Update ---

// Note appends a capped breadcrumb.
func (s *State) Note(format string, args ...any) {
	s.Notes = append(s.Notes, time.Now().UTC().Format("15:04:05")+" "+fmt.Sprintf(format, args...))
	if len(s.Notes) > maxNotes {
		s.Notes = s.Notes[len(s.Notes)-maxNotes:]
	}
}

// IsDone reports whether an issue already completed in this run.
func (s *State) IsDone(id string) bool {
	for _, d := range s.Done {
		if d == id {
			return true
		}
	}
	return false
}

// IsParked reports whether an issue was set aside.
func (s *State) IsParked(id string) bool {
	for _, p := range s.Parked {
		if p.ID == id {
			return true
		}
	}
	return false
}

// Excluded reports whether an issue should be kept out of a new wave.
func (s *State) Excluded(id string) bool {
	if s.IsDone(id) || s.IsParked(id) {
		return true
	}
	_, inflight := s.InFlight[id]
	return inflight
}

// MarkDone records a completed issue and clears its in-flight entry.
func (s *State) MarkDone(id string) {
	delete(s.InFlight, id)
	if !s.IsDone(id) {
		s.Done = append(s.Done, id)
	}
}

// Park sets an issue aside after its attempts are exhausted.
func (s *State) Park(id, reason, stage string) {
	delete(s.InFlight, id)
	if s.IsParked(id) {
		return
	}
	s.Parked = append(s.Parked, Parked{
		ID:       id,
		Reason:   reason,
		Attempts: s.Attempts[id],
		Stage:    stage,
		ParkedAt: time.Now().UTC(),
	})
}

// IssueForAgent resolves a subagent's agent_id to the issue it claimed.
func (s *State) IssueForAgent(agentID string) (string, bool) {
	id, ok := s.Bindings[agentID]
	return id, ok
}

// Remaining reports issues in the current wave still in flight.
func (s *State) Remaining() []string {
	out := []string{}
	for id := range s.InFlight {
		out = append(out, id)
	}
	return out
}
