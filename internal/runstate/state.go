// Package runstate owns .beads/auto/run.json, the durable state of a bd-auto
// run.
//
// This file is the reason an interrupted run resumes rather than restarts:
// everything needed to pick it up again is here, not in a context window. It is
// also written concurrently — by parallel issue goroutines inside one drain, and
// by other bd-auto processes reading and writing the same repo — so every
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
	"strings"
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
	// WorkerSession is the model session implementing this attempt, and
	// ReviewSession the one judging it. Both are written by the engine before
	// the runner is invoked rather than after it returns, which is what makes an
	// interrupted attempt resumable: a session recorded only once the process
	// exits is lost by exactly the failure that needs it.
	WorkerSession string `json:"worker_session,omitempty"`
	// ReviewSession is the reviewer's session for this attempt. It is normally
	// short-lived, since a reviewer defaults to judging fresh.
	ReviewSession string    `json:"review_session,omitempty"`
	Branch        string    `json:"branch"`
	Attempt       int       `json:"attempt"`
	StartedAt     time.Time `json:"started_at"`
	Stage         string    `json:"stage,omitempty"`
	Rounds        int       `json:"rounds,omitempty"`
}

// Failure is why the last attempt at an issue failed, kept so the next one can
// be told.
//
// bd-auto keeps its own copy rather than reading the note back off the issue,
// because that note does not survive. beads installs a post-checkout hook that
// imports .beads/issues.jsonl over its database, so creating the next attempt's
// worktree reverts every bd write made since the worker's last commit — the
// failure note among them, since it is written after that commit and never
// committed itself. run.json is under .beads/auto/, which is gitignored and
// therefore neither exported nor imported over, so what is written here is
// still here on the other side of a checkout.
type Failure struct {
	// Attempt is the attempt that failed, and Of how many it was allowed.
	Attempt int    `json:"attempt"`
	Of      int    `json:"of,omitempty"`
	Stage   string `json:"stage,omitempty"`
	// Reason is the failure itself, in the same words the worker would have
	// read off the issue.
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Summary renders a failure as the one-line-plus-body form the retry note uses.
func (f Failure) Summary() string {
	head := fmt.Sprintf("attempt %d", f.Attempt)
	if f.Of > 0 {
		head += fmt.Sprintf("/%d", f.Of)
	}
	if f.Stage != "" {
		head += fmt.Sprintf(" failed at stage %q", f.Stage)
	} else {
		head += " failed"
	}
	if f.Reason == "" {
		return head
	}
	return head + ":\n" + f.Reason
}

// Question is something a model asked the human during this run, and what it
// was told.
//
// It is kept for two reasons, and the second is the one that pays for the
// field. A human reading a finished run can see which decisions were theirs
// rather than the model's — but more than that, a run that is interrupted and
// re-run must not ask the same question twice. The worker is a fresh process
// with no memory of having asked, so the only place that memory can live is
// here.
//
// Only answers a human actually gave are recorded. A question nobody answered
// is not an answered question, and writing "nobody was there" down would
// suppress it on a later run where somebody is.
type Question struct {
	ID    string `json:"id"`
	Issue string `json:"issue"`
	Role  string `json:"role,omitempty"`
	// Question is the text as the model asked it, and Options what it offered.
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	// Answer is what went back, and Source who decided it.
	Answer     string    `json:"answer"`
	Source     string    `json:"source"`
	AskedAt    time.Time `json:"asked_at"`
	AnsweredAt time.Time `json:"answered_at"`
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

	// Scope is the set of issues a human selected before anything was spawned,
	// and it is a hard allowlist for the run's whole life. An empty Scope is an
	// unrestricted run — everything under the epic — never an empty one, because
	// a run recorded before scope selection existed has no list to read.
	Scope []string `json:"scope,omitempty"`

	// Base is the branch the run started on: the branch a pull request targets,
	// and the branch the run leaves untouched while it is staged. It is
	// recorded at the first barrier, because that is the last moment it is
	// still readable — from then on the main checkout is on the epic branch.
	Base string `json:"base,omitempty"`
	// EpicBranch is the temporary branch this run's merges are staged on. It is
	// minted once and reused by every later barrier: a worker branches from the
	// main checkout's HEAD, so a run that moved off this branch between waves
	// would silently drop everything already merged.
	EpicBranch string `json:"epic_branch,omitempty"`
	// PR is the pull request this run was handed over as, empty until one is
	// opened.
	PR string `json:"pr,omitempty"`

	Wave       int      `json:"wave"`
	WaveIssues []string `json:"wave_issues"`

	// InFlight maps issue ID to the attempt currently working it.
	InFlight map[string]Attempt `json:"in_flight"`
	// Attempts counts total attempts per issue across the run.
	Attempts map[string]int `json:"attempts"`
	// Failures maps issue ID to the last attempt at it that failed. It is what
	// a fresh retry is told about the attempt before it, and it is here rather
	// than on the issue because the copy on the issue does not survive the next
	// worktree being created. See Failure.
	Failures map[string]Failure `json:"failures,omitempty"`

	Done   []string `json:"done"`
	Parked []Parked `json:"parked"`

	// Questions is what this run's models asked the human, and what they were
	// told. See Question.
	Questions []Question `json:"questions,omitempty"`

	// LastWaveChange is the wave number at the last time the wave advanced.
	LastWaveChange int `json:"last_wave_change"`

	// Notes is a short human-readable breadcrumb trail, capped.
	Notes []string `json:"notes,omitempty"`
}

const maxNotes = 40

// maxQuestions caps the recorded questions. It is generous rather than tight:
// every entry is one a human typed an answer into, so there will not be many,
// and dropping one costs that answer being asked for again.
const maxQuestions = 200

// Dir returns the run-state directory for a repo.
func Dir(repoRoot string) string { return filepath.Join(repoRoot, ".beads", "auto") }

// Path returns the run-state file for a repo.
func Path(repoRoot string) string { return filepath.Join(Dir(repoRoot), "run.json") }

func lockPath(repoRoot string) string { return filepath.Join(Dir(repoRoot), "run.lock") }

// Active reports whether a run is in progress. Anything that would disturb a
// live drain — a script that tears down .beads/auto, a second drain — checks
// this first and refuses rather than clobbering it.
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
	if s.Attempts == nil {
		s.Attempts = map[string]int{}
	}
	if s.Failures == nil {
		s.Failures = map[string]Failure{}
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

// Clear removes the run state, so the next drain starts fresh rather than
// resuming this one.
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

// InScope reports whether an issue is one the run was allowed to touch. A run
// with no recorded scope was not restricted, so everything is in it.
func (s *State) InScope(id string) bool {
	if len(s.Scope) == 0 {
		return true
	}
	for _, x := range s.Scope {
		if x == id {
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
	delete(s.Failures, id)
	if !s.IsDone(id) {
		s.Done = append(s.Done, id)
	}
}

// RecordFailure keeps the last failed attempt at an issue, replacing whatever
// an earlier attempt left. Only the last one is kept: it is what the next
// attempt needs, and an unbounded list in a file every worker rewrites is a
// cost with no reader.
func (s *State) RecordFailure(id string, f Failure) {
	if f.At.IsZero() {
		f.At = time.Now().UTC()
	}
	s.Failures[id] = f
}

// LastFailure returns what this run recorded about the last failed attempt at
// an issue, and whether there was one.
func (s *State) LastFailure(id string) (Failure, bool) {
	f, ok := s.Failures[id]
	return f, ok
}

// Park sets an issue aside after its attempts are exhausted.
//
// It also drops the issue from Done, because an issue can be one or the other
// and never both: a branch that was parked at the barrier did not land, whatever
// the worker that produced it reported.
func (s *State) Park(id, reason, stage string) {
	delete(s.InFlight, id)
	for i, d := range s.Done {
		if d == id {
			s.Done = append(s.Done[:i], s.Done[i+1:]...)
			break
		}
	}
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

// Unpark returns a parked issue to the run and resets its attempt count, so the
// next wave offers it again with a full retry budget. Reports whether the issue
// was actually parked.
func (s *State) Unpark(id string) bool {
	for i, p := range s.Parked {
		if p.ID != id {
			continue
		}
		s.Parked = append(s.Parked[:i], s.Parked[i+1:]...)
		delete(s.Attempts, id)
		// The attempt count is what makes a carried failure make sense, so the
		// two are reset together: an unparked issue starts at attempt one, and
		// attempt one is not told about a previous attempt that no longer
		// counts against it.
		delete(s.Failures, id)
		return true
	}
	return false
}

// RecordQuestion keeps an answered question, replacing an earlier answer to the
// same question from the same issue rather than appending beside it: the last
// answer a human gave is the one that stands.
func (s *State) RecordQuestion(q Question) {
	if q.AnsweredAt.IsZero() {
		q.AnsweredAt = time.Now().UTC()
	}
	key := normaliseQuestion(q.Question)
	for i, prev := range s.Questions {
		if prev.Issue == q.Issue && normaliseQuestion(prev.Question) == key {
			s.Questions[i] = q
			return
		}
	}
	s.Questions = append(s.Questions, q)
	if len(s.Questions) > maxQuestions {
		s.Questions = s.Questions[len(s.Questions)-maxQuestions:]
	}
}

// AnswerFor returns what this run already recorded for a question, matched on
// the issue and the words asked, whatever the spacing or case.
//
// It is per-issue on purpose. The same sentence asked by two different issues
// is two different questions — "which error type should this return" has an
// answer that belongs to the code being written, not to the phrasing.
func (s *State) AnswerFor(issue, question string) (Question, bool) {
	key := normaliseQuestion(question)
	for i := len(s.Questions) - 1; i >= 0; i-- {
		q := s.Questions[i]
		if q.Issue == issue && normaliseQuestion(q.Question) == key && q.Answer != "" {
			return q, true
		}
	}
	return Question{}, false
}

// normaliseQuestion reduces a question to what makes two of them the same one.
func normaliseQuestion(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

// Remaining reports issues in the current wave still in flight.
func (s *State) Remaining() []string {
	out := []string{}
	for id := range s.InFlight {
		out = append(out, id)
	}
	return out
}
