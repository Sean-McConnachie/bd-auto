package bd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is fixed because deferral is a date comparison: a test that read the
// real clock would start passing or failing on the calendar rather than on the
// code.
var clock = time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)

// children is one epic's listing as bd prints it, with the two shapes of
// defer_until bd emits: a date still ahead, and one already behind.
const children = `[
  {"id":"a","status":"closed"},
  {"id":"b","status":"open"},
  {"id":"c","status":"open","defer_until":"2029-05-13T13:04:03Z"},
  {"id":"d","status":"blocked"},
  {"id":"e","status":"open","defer_until":"2020-01-01T00:00:00Z"},
  {"id":"f","status":"closed","defer_until":"2029-05-13T08:46:32Z"}
]`

// stubList puts a bd on PATH that answers any `list` with a fixture and records
// the arguments it was called with.
func stubList(t *testing.T, listing string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	fixture := filepath.Join(dir, "listing.json")
	if err := os.WriteFile(fixture, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(dir, "argv")
	path := filepath.Join(dir, "bd-stub")
	script := "#!/usr/bin/env sh\n" +
		"echo \"$@\" >> " + argv + "\n" +
		"if [ \"$1\" = \"list\" ]; then cat " + fixture + "; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := Binary
	Binary = path
	t.Cleanup(func() { Binary = prev })
	return dir
}

// bd does not subtract deferred work from what it reports — measured in this
// repo on 2026-08-18, `bd list --all` returned 19 open issues, 16 of them
// deferred to 2029, and `bd stats` called all 19 ready to work — so run status
// counts them itself or reports work nobody can pick up.
func TestEpicStatsCountsDeferredApartFromOpen(t *testing.T) {
	dir := stubList(t, children)
	got, err := New(dir).EpicStats("epic-1", clock)
	if err != nil {
		t.Fatal(err)
	}
	// e's defer date has passed, so bd offers it again and so does this. f is
	// closed and deferred, which is common for discovered work that was later
	// fixed; closed wins.
	want := Stats{Total: 6, Open: 2, Closed: 2, Blocked: 1, Deferred: 1}
	if got != want {
		t.Fatalf("stats %+v, want %+v", got, want)
	}
}

// All is what discovery deduplication is checked against, and a deferred issue
// is exactly the kind a worker rediscovers: an earlier run filed it and nobody
// has looked at it yet. Filtering it out here would refile it every run, so
// this pins that nothing ever does.
func TestAllStillReturnsDeferredIssues(t *testing.T) {
	dir := stubList(t, children)
	all, err := New(dir).All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("All returned %d issues, want every one bd listed (6)", len(all))
	}
	for _, id := range []string{"c", "f"} {
		var found *Issue
		for i := range all {
			if all[i].ID == id {
				found = &all[i]
			}
		}
		if found == nil {
			t.Fatalf("deferred issue %s is missing from All", id)
		}
		if found.DeferUntil.IsZero() {
			t.Fatalf("%s: defer_until did not decode", id)
		}
	}
}

func TestDeferred(t *testing.T) {
	cases := []struct {
		name  string
		until time.Time
		want  bool
	}{
		{"never deferred", time.Time{}, false},
		{"deferred past the clock", clock.AddDate(3, 0, 0), true},
		{"the date has come round", clock.AddDate(-1, 0, 0), false},
		// The boundary goes to not-deferred: at the instant its date arrives bd
		// puts the issue back in ready, and disagreeing would park work bd is
		// offering.
		{"exactly now", clock, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Issue{DeferUntil: tc.until}).Deferred(clock); got != tc.want {
				t.Fatalf("Issue.Deferred = %v, want %v", got, tc.want)
			}
			if got := (Ref{DeferUntil: tc.until}).Deferred(clock); got != tc.want {
				t.Fatalf("Ref.Deferred = %v, want %v", got, tc.want)
			}
		})
	}
}
