package drain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bd-auto/internal/bd"
	"bd-auto/internal/runstate"
)

// Discovered work: found by a worker, filed by the barrier.
//
// # Why the worker does not file it
//
// It used to. Step 6 of the worker prompt ran `bd create` directly, and that
// had two faults which are really the same fault — a worker is alone.
//
// It cannot see what any other worker has filed, so a fault that several issues
// touch is filed once per worker that trips over it. Two of this repo's own
// open issues are that: the same "drop the plugin-era role aliases", filed by
// two workers three waves apart, in almost the same words.
//
// And it is a bd write from inside a worktree, which is the class of write
// beads' import hooks revert (see internal/gitx). A discovery that a
// post-checkout hook rolled back is a discovery nobody ever reads.
//
// # The shape
//
// A worker writes JSON to a path bd-auto gives it in its task. bd-auto harvests
// that file at the end of every attempt into run state, and files what it holds
// at the barrier, deduplicated against both the rest of the run and everything
// already in bd.
//
// The file is in the MAIN checkout rather than in the worktree, and that is
// deliberate: nothing a worker's `git add -A` can reach can end up committed to
// its branch by accident. It is per-issue, so two workers running at once never
// write the same file.

// discoveredDir is where workers leave what they found, under the main
// checkout's already-gitignored run directory.
const discoveredDir = "discovered"

// DiscoveriesPath returns the file the worker on an issue writes its findings
// to. It is an absolute path, because it is handed to a model whose working
// directory is a worktree somewhere else entirely.
func DiscoveriesPath(repoRoot, issue string) string {
	return filepath.Join(runstate.Dir(repoRoot), discoveredDir, safeName(issue)+".json")
}

// discoveryFile is the JSON contract with the worker: a list, or an object with
// a "discovered" list. Both are accepted because both are what a model writes
// when told to write a list, and failing on the second one would cost a round
// to teach it a distinction with no meaning.
type discoveryFile struct {
	Discovered []discoveryEntry `json:"discovered"`
}

type discoveryEntry struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type,omitempty"`
	Priority    string `json:"priority,omitempty"`
}

// knownTypes are the issue types bd accepts. A worker that invents one gets the
// default rather than a failed create at the barrier: the title and the
// description are the valuable part, and the type is a label a human will
// change anyway.
var knownTypes = map[string]bool{
	"bug": true, "feature": true, "task": true,
	"epic": true, "chore": true, "decision": true,
}

// harvest reads what the worker on an issue left behind, records it in run
// state, and removes the file.
//
// Removing it is what keeps an attempt's findings its own. The file is read at
// the end of every attempt whatever the outcome — a failed attempt still did
// the exploring, and an attempt that stopped on the environment did nothing
// wrong at all — but the next attempt starts from an empty file rather than
// re-harvesting the last one's.
//
// Nothing here is fatal. A malformed file costs the findings in it and a line
// in the log; it never costs the attempt, which has already finished.
func (e *Engine) harvest(issue string) {
	path := DiscoveriesPath(e.RepoRoot, issue)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	defer os.Remove(path)
	if err != nil {
		e.logf("warning: could not read what %s discovered: %v", issue, err)
		return
	}

	entries, err := parseDiscoveries(raw)
	if err != nil {
		e.logf("warning: %s left an unreadable discoveries file: %v", issue, err)
		return
	}
	if len(entries) == 0 {
		return
	}

	var added, dropped int
	_, err = runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		for _, ent := range entries {
			d, ok := discoveryOf(issue, ent)
			if !ok {
				dropped++
				continue
			}
			if s.AddDiscovery(d) {
				added++
			}
		}
		return nil
	})
	if err != nil {
		e.logf("warning: could not record what %s discovered: %v", issue, err)
		return
	}
	if dropped > 0 {
		e.logf("%s: dropped %d discovery entr(ies) with no title or no description", issue, dropped)
	}
	if added > 0 {
		e.logf("%s: recorded %d discover(ies) for the barrier to file", issue, added)
	}
}

// parseDiscoveries accepts either shape of the file.
func parseDiscoveries(raw []byte) ([]discoveryEntry, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil, nil
	}
	var list []discoveryEntry
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped discoveryFile
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Discovered, nil
}

// discoveryOf validates one entry. A title and a description are both required:
// the bar the worker prompt sets is that a discovery is something a human would
// schedule, and a title with nothing under it fails that on its face.
func discoveryOf(from string, ent discoveryEntry) (runstate.Discovery, bool) {
	title := strings.TrimSpace(ent.Title)
	desc := strings.TrimSpace(ent.Description)
	if title == "" || desc == "" {
		return runstate.Discovery{}, false
	}
	d := runstate.Discovery{From: from, Title: title, Description: desc}
	if t := strings.ToLower(strings.TrimSpace(ent.Type)); knownTypes[t] {
		d.Type = t
	}
	if p := normalisePriority(ent.Priority); p != "" {
		d.Priority = p
	}
	return d, true
}

// normalisePriority accepts bd's two spellings and rejects everything else,
// returning "" for bd's own default.
func normalisePriority(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "p")
	switch s {
	case "0", "1", "2", "3", "4":
		return s
	}
	return ""
}

// --- filing, at the barrier ---

// DiscoveryFiling is what one barrier did with the run's findings.
type DiscoveryFiling struct {
	// Filed maps each new issue's ID to the issue whose worker found it.
	Filed map[string]string `json:"filed,omitempty"`
	// Skipped counts findings bd already had, and Failed those bd refused.
	Skipped int `json:"skipped,omitempty"`
	Failed  int `json:"failed,omitempty"`
}

// Empty reports whether the barrier had nothing to file.
func (f DiscoveryFiling) Empty() bool {
	return len(f.Filed) == 0 && f.Skipped == 0 && f.Failed == 0
}

// DiscoveredLabel marks every issue bd-auto files on a worker's behalf, so the
// backlog a run generated can be told from the backlog a human wrote.
const DiscoveredLabel = "discovered"

// DiscoveredDefer is how long a discovered issue is hidden from bd ready.
//
// The run works on a fixed set of issues a human approved, and discovered work
// waits for a human rather than joining it. The deferral is what enforces that
// in bd itself, on top of the run's own scope allowlist — belt and braces,
// because the two protect against different mistakes: the allowlist stops this
// run, and the deferral stops the next one.
const DiscoveredDefer = "+1000d"

// fileDiscoveries files what this run's workers found, once each.
//
// Deduplication happens twice, against two different things. Run state already
// refuses a second copy of a title this run has seen, which covers two workers
// finding the same fault. Here it is checked against bd itself, which covers
// the case run state cannot see: a finding that was already filed by an earlier
// run, or by a human, or filed and already fixed.
//
// Filing is deliberately at the barrier rather than as each issue finishes. The
// barrier is the one point where the run's findings are complete for the wave,
// and comparing a finding against bd is a query worth making once per wave
// rather than once per worker.
func (e *Engine) fileDiscoveries() DiscoveryFiling {
	var out DiscoveryFiling

	st, err := runstate.Load(e.RepoRoot)
	if err != nil {
		e.logf("warning: could not re-read the run state to file discoveries: %v", err)
		return out
	}
	pending := st.PendingDiscoveries()
	if len(pending) == 0 {
		return out
	}

	// One query for the whole wave. A bd that cannot be listed is a reason to
	// file nothing rather than to file everything twice: a duplicate issue
	// costs a human's attention permanently, and a delayed one costs a wave.
	existing, err := e.existingTitles()
	if err != nil {
		e.logf("warning: could not read bd to deduplicate %d discover(ies); leaving them for the next barrier: %v",
			len(pending), err)
		return out
	}

	deferred := e.Cfg.DeferDiscovered()
	deferUntil := ""
	if deferred {
		deferUntil = DiscoveredDefer
	}

	for _, d := range pending {
		if prior, dup := existing[d.Key()]; dup {
			e.logf("%s: not filing %q; bd already has it as %s", d.From, d.Title, prior)
			e.resolveDiscovery(d.Key(), "", "bd already had this as "+prior)
			out.Skipped++
			continue
		}
		id, err := e.BD.Create(bd.NewIssue{
			Title:       d.Title,
			Description: discoveredDescription(d, deferred),
			Type:        d.Type,
			Priority:    d.Priority,
			Labels:      []string{DiscoveredLabel},
			Deps:        []string{"discovered-from:" + d.From},
			Defer:       deferUntil,
		})
		if err != nil {
			e.logf("warning: could not file %q discovered by %s: %v", d.Title, d.From, err)
			out.Failed++
			continue
		}
		// Recorded immediately, one at a time. A barrier killed halfway through
		// filing must not re-file what it already created on the next run.
		e.resolveDiscovery(d.Key(), id, "")
		if out.Filed == nil {
			out.Filed = map[string]string{}
		}
		out.Filed[id] = d.From
		existing[d.Key()] = id
		e.logf("filed %s: %q, discovered by %s", id, d.Title, d.From)
	}
	return out
}

// existingTitles indexes bd by the same key a discovery is compared on.
func (e *Engine) existingTitles() (map[string]string, error) {
	all, err := e.BD.All()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for _, iss := range all {
		if iss.Title == "" {
			continue
		}
		key := runstate.NormaliseTitle(iss.Title)
		// First writer wins, so the ID reported for a duplicate is a stable
		// choice rather than whichever bd happened to list last.
		if _, seen := out[key]; !seen {
			out[key] = iss.ID
		}
	}
	return out, nil
}

func (e *Engine) resolveDiscovery(key, filedAs, skipped string) {
	if _, err := runstate.Update(e.RepoRoot, false, func(s *runstate.State) error {
		s.ResolveDiscovery(key, filedAs, skipped)
		return nil
	}); err != nil {
		e.logf("warning: could not record what became of a discovery: %v", err)
	}
}

// discoveredDescription is the worker's description with its provenance under
// it. The provenance is the first thing a human triaging this backlog needs and
// the first thing they cannot reconstruct: which piece of work was in front of
// somebody when they noticed this.
func discoveredDescription(d runstate.Discovery, deferred bool) string {
	tail := "deferred, so it waits for a human rather than being offered to the next run"
	if !deferred {
		tail = "and offered to bd ready straight away, because this repo sets " +
			"discovered_work: immediate"
	}
	return fmt.Sprintf("%s\n\nFound by the bd-auto worker on %s, and filed at the wave barrier — %s.",
		d.Description, d.From, tail)
}
