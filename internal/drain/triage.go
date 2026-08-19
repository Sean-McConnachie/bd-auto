package drain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
	"bd-auto/internal/similar"
)

// The triage gate.
//
// # What it is for
//
// Measured over this repository's whole history, agent-discovered work peaked
// at 2.27 issues created per issue closed. The shape of the discovery edges
// says what that was: nine different parent issues each produced exactly two
// children. Real discovery is lumpy. A constant two per issue is a model
// answering a question it is expected to have an answer to.
//
// beads-auto-imp-fqu moved filing from the worker to the barrier, so a run at
// least files each finding once. beads-auto-imp-qhs put a bar in the worker
// prompt, so a worker is told what does not clear it. Both helped and neither
// touched the thing that actually grows the backlog: the barrier still runs
// `bd create`, so the issue count goes up whether or not anything was learned.
// DiscoveredDefer hides the result from `bd ready`, which protects the next
// run — but the list a human reads is the list that grows.
//
// Under `discovered_work: triage` the barrier files nothing. Findings land
// here, in a file that outlives the run that found them, and a human turns one
// into an issue, folds it into an issue that already exists, or discards it.
//
// # Why it is a file and not run state
//
// A discovery outlives the run that found it, and run state does not: `run
// stop` clears it, the next drain replaces it. A finding that a human has not
// looked at yet must survive both.
//
// # Three outcomes, not two
//
// A discarded discovery is kept, with its reason. The record of what a run
// decided not to file is the only evidence there is that the bar is set in the
// right place, and it is the first thing to read when the backlog starts
// growing again.

// TriageFile is where staged discoveries live, under the main checkout's
// already-gitignored run directory.
func TriageFile(repoRoot string) string {
	return filepath.Join(runstate.Dir(repoRoot), "triage.json")
}

// Staged is one discovery waiting for a human.
type Staged struct {
	// Key identifies it, and is what the triage commands take. It is derived
	// from the title so a human can retype it.
	Key string `json:"key"`
	// From is the issue whose worker found this; Run is when that run started,
	// so a stale finding can be told from a fresh one.
	From  string    `json:"from"`
	Run   time.Time `json:"run,omitempty"`
	Found time.Time `json:"found"`

	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type,omitempty"`
	Priority    string `json:"priority,omitempty"`

	// Resembles and Score are what the backlog already had that looks like
	// this, when anything did. It is a hint for the human, not a decision: see
	// similar.HintThreshold.
	Resembles string  `json:"resembles,omitempty"`
	Score     float64 `json:"score,omitempty"`

	// Outcome is empty while this is pending, and otherwise one of filed,
	// merged or discarded. FiledAs, MergedInto and Reason carry the detail.
	Outcome    string    `json:"outcome,omitempty"`
	FiledAs    string    `json:"filed_as,omitempty"`
	MergedInto string    `json:"merged_into,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	DecidedAt  time.Time `json:"decided_at,omitempty"`
}

// Pending reports whether a human has not yet decided about this.
func (s Staged) Pending() bool { return s.Outcome == "" }

// Triage is the whole staging file.
type Triage struct {
	Version int      `json:"version"`
	Staged  []Staged `json:"staged"`
}

// LoadTriage reads the staging file. A repo that has never staged anything
// reads as empty rather than as an error.
func LoadTriage(repoRoot string) (*Triage, error) {
	raw, err := os.ReadFile(TriageFile(repoRoot))
	if os.IsNotExist(err) {
		return &Triage{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var t Triage
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", TriageFile(repoRoot), err)
	}
	if t.Version == 0 {
		t.Version = 1
	}
	return &t, nil
}

// Save writes the staging file atomically, so a barrier killed mid-write leaves
// the previous list rather than half of the new one.
func (t *Triage) Save(repoRoot string) error {
	path := TriageFile(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Pending is everything still waiting for a human, oldest first.
func (t *Triage) Pending() []Staged {
	var out []Staged
	for _, s := range t.Staged {
		if s.Pending() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Found.Before(out[j].Found) })
	return out
}

// Find returns the pending entry a key names. A key may be given in full or as
// a unique prefix, because the full key is a whole issue title.
//
// An ambiguous prefix is an error rather than a guess: the commands this backs
// file issues and write notes, and picking the wrong one of two findings is not
// something a human would notice.
func (t *Triage) Find(key string) (*Staged, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return nil, errors.New("no discovery named")
	}
	var hits []int
	for i := range t.Staged {
		if !t.Staged[i].Pending() {
			continue
		}
		if t.Staged[i].Key == key {
			return &t.Staged[i], nil
		}
		if strings.HasPrefix(t.Staged[i].Key, key) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no pending discovery matches %q", key)
	case 1:
		return &t.Staged[hits[0]], nil
	}
	var titles []string
	for _, i := range hits {
		titles = append(titles, t.Staged[i].Title)
	}
	return nil, fmt.Errorf("%q matches %d pending discoveries, so it is ambiguous:\n  %s",
		key, len(hits), strings.Join(titles, "\n  "))
}

// Add stages a discovery unless this file already holds it, decided or not.
//
// Already holding a discarded copy counts: re-staging something a human has
// already said no to is how a rejected finding comes back every run.
func (t *Triage) Add(s Staged) bool {
	for _, prev := range t.Staged {
		if prev.Key == s.Key {
			return false
		}
	}
	t.Staged = append(t.Staged, s)
	return true
}

// --- the barrier's half ---

// stageDiscoveries puts this run's findings in the staging file instead of
// filing them, and records against each one whatever in bd already looks like
// it.
//
// It reports the same DiscoveryFiling shape a filing barrier does, with Staged
// counted rather than Filed, so the run summary and the event stream do not
// have to know which mode they are in.
func (e *Engine) stageDiscoveries() DiscoveryFiling {
	var out DiscoveryFiling

	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		e.logf("warning: could not re-read the run state to stage discoveries: %v", err)
		return out
	}
	pending := st.PendingDiscoveries()
	if len(pending) == 0 {
		return out
	}

	ix, err := e.backlogIndex()
	if err != nil {
		// Staging without the hint is worth much more than not staging: the
		// finding is kept either way, and the hint is a convenience for the
		// human who reads it.
		e.logf("warning: could not read bd to look for lookalikes: %v", err)
	}

	t, err := LoadTriage(e.RepoRoot)
	if err != nil {
		e.logf("warning: could not read the triage file; leaving %d discover(ies) for the next barrier: %v",
			len(pending), err)
		return out
	}

	for _, d := range pending {
		s := Staged{
			Key:         d.Key(),
			From:        d.From,
			Run:         st.StartedAt,
			Found:       d.FoundAt,
			Title:       d.Title,
			Description: d.Description,
			Type:        d.Type,
			Priority:    d.Priority,
		}
		if s.Found.IsZero() {
			s.Found = time.Now().UTC()
		}
		if ix != nil {
			if m := ix.Best(d.Title + "\n\n" + d.Description); m.Score >= similar.DefaultThreshold {
				// Strong enough to decide without a human. Recorded as
				// discarded rather than dropped, so the decision is visible.
				e.logf("%s: not staging %q; bd already has it as %s (%.2f)",
					d.From, d.Title, m.ID, m.Score)
				e.resolveDiscovery(d.Key(), "", fmt.Sprintf("bd already had this as %s", m.ID))
				s.Outcome, s.Reason, s.Resembles, s.Score = "discarded", "already in bd as "+m.ID, m.ID, m.Score
				s.DecidedAt = time.Now().UTC()
				t.Add(s)
				out.Skipped++
				continue
			} else if m.Score >= similar.HintThreshold {
				s.Resembles, s.Score = m.ID, m.Score
			}
		}
		if !t.Add(s) {
			e.logf("%s: %q is already staged", d.From, d.Title)
			e.resolveDiscovery(d.Key(), "", "already staged for triage")
			out.Skipped++
			continue
		}
		e.resolveDiscovery(d.Key(), "", "staged for triage")
		out.Staged++
		if s.Resembles != "" {
			e.logf("staged %q from %s — looks like %s (%.2f)", d.Title, d.From, s.Resembles, s.Score)
		} else {
			e.logf("staged %q, discovered by %s", d.Title, d.From)
		}
	}

	if err := t.Save(e.RepoRoot); err != nil {
		e.logf("warning: could not write the triage file: %v", err)
	}
	return out
}

// backlogIndex weighs every issue bd has, open or closed.
//
// Closed issues are in it deliberately. A finding that duplicates something
// already fixed is exactly as unwanted as one that duplicates something open,
// and it is the one a human is least able to recognise.
func (e *Engine) backlogIndex() (*similar.Index, error) {
	all, err := e.BD.All()
	if err != nil {
		return nil, err
	}
	docs := make([]similar.Doc, 0, len(all))
	for _, iss := range all {
		if iss.Title == "" {
			continue
		}
		docs = append(docs, similar.Doc{ID: iss.ID, Text: iss.Title + "\n\n" + iss.Description})
	}
	return similar.NewIndex(docs), nil
}

// --- the human's half ---

// TriageDecision is what one accept, merge or discard did.
type TriageDecision struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Outcome    string `json:"outcome"`
	FiledAs    string `json:"filed_as,omitempty"`
	MergedInto string `json:"merged_into,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Accept files a staged discovery as an issue, with the discovered-from edge
// the barrier used to add and the same label, so a triaged backlog is still
// distinguishable from a hand-written one.
func (e *Engine) Accept(t *Triage, key string) (TriageDecision, error) {
	s, err := t.Find(key)
	if err != nil {
		return TriageDecision{}, err
	}
	deps := []string{}
	if s.From != "" {
		deps = append(deps, "discovered-from:"+s.From)
	}
	id, err := e.BD.Create(bd.NewIssue{
		Title:       s.Title,
		Description: triagedDescription(*s),
		Type:        s.Type,
		Priority:    s.Priority,
		Labels:      []string{DiscoveredLabel},
		Deps:        deps,
	})
	if err != nil {
		return TriageDecision{}, fmt.Errorf("could not file %q: %w", s.Title, err)
	}
	s.Outcome, s.FiledAs, s.DecidedAt = "filed", id, time.Now().UTC()
	if err := t.Save(e.RepoRoot); err != nil {
		// The issue exists. Saying so is more useful than failing, because a
		// retry would file it twice.
		return TriageDecision{Key: s.Key, Title: s.Title, Outcome: "filed", FiledAs: id},
			fmt.Errorf("filed %s, but could not record it in the triage file: %w", id, err)
	}
	return TriageDecision{Key: s.Key, Title: s.Title, Outcome: "filed", FiledAs: id}, nil
}

// Merge folds a staged discovery into an issue that already exists, as a note.
// Nothing is created.
func (e *Engine) Merge(t *Triage, key, into string) (TriageDecision, error) {
	s, err := t.Find(key)
	if err != nil {
		return TriageDecision{}, err
	}
	if strings.TrimSpace(into) == "" {
		return TriageDecision{}, errors.New("--into needs the issue to fold this into")
	}
	target, err := e.BD.Show(into)
	if err != nil {
		return TriageDecision{}, fmt.Errorf("could not read %s to fold into it: %w", into, err)
	}
	note := fmt.Sprintf("Also found by the bd-auto worker on %s: %s\n\n%s",
		s.From, s.Title, s.Description)
	if err := e.BD.AppendNotes(target.ID, note); err != nil {
		return TriageDecision{}, fmt.Errorf("could not append to %s: %w", target.ID, err)
	}
	s.Outcome, s.MergedInto, s.DecidedAt = "merged", target.ID, time.Now().UTC()
	if err := t.Save(e.RepoRoot); err != nil {
		return TriageDecision{Key: s.Key, Title: s.Title, Outcome: "merged", MergedInto: target.ID},
			fmt.Errorf("appended to %s, but could not record it in the triage file: %w", target.ID, err)
	}
	return TriageDecision{Key: s.Key, Title: s.Title, Outcome: "merged", MergedInto: target.ID}, nil
}

// Discard records that a human read this and does not want it. The reason is
// required: a discard with no reason is indistinguishable from a lost finding.
func (e *Engine) Discard(t *Triage, key, reason string) (TriageDecision, error) {
	s, err := t.Find(key)
	if err != nil {
		return TriageDecision{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return TriageDecision{}, errors.New("--reason says why this is not work, and is what makes " +
			"a discard readable later; a discard with no reason cannot be told from a lost finding")
	}
	s.Outcome, s.Reason, s.DecidedAt = "discarded", reason, time.Now().UTC()
	if err := t.Save(e.RepoRoot); err != nil {
		return TriageDecision{}, err
	}
	return TriageDecision{Key: s.Key, Title: s.Title, Outcome: "discarded", Reason: reason}, nil
}

// triagedDescription is the worker's description with its provenance under it:
// which piece of work was in front of somebody when they noticed this, which is
// the thing a reader cannot reconstruct.
func triagedDescription(s Staged) string {
	return fmt.Sprintf("%s\n\nFound by the bd-auto worker on %s, staged for triage, and filed "+
		"by a human who read it.", s.Description, s.From)
}
