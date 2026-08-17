package drain

import (
	"strings"
	"testing"
)

// The default forge has to say no before it does anything, because a repo with
// no remote is a perfectly good repo to drain and must not become a failed run.
func TestGHRefusesARepoWithNoRemote(t *testing.T) {
	repo := testRepo(t)
	why := GH{}.Available(repo, "origin")
	if why == "" {
		t.Fatal("a repo with no remote must not be reported as ready to hand over")
	}
	// One of the two reasons, depending on whether this machine has gh at all.
	if !strings.Contains(why, "origin") && !strings.Contains(why, "gh CLI") {
		t.Fatalf("the refusal explains nothing actionable: %q", why)
	}
}

// gh prints the pull request URL on its own line, around whatever advisory
// noise it feels like adding to stdout.
func TestFirstURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/pull/12":                                  "https://github.com/o/r/pull/12",
		"Warning: 3 uncommitted changes\nhttps://github.com/o/r/pull/9\n": "https://github.com/o/r/pull/9",
		"  https://example.invalid/x  ":                                   "https://example.invalid/x",
		"nothing useful here":                                             "",
		"":                                                                "",
	}
	for in, want := range cases {
		if got := firstURL(in); got != want {
			t.Fatalf("firstURL(%q) = %q, want %q", in, got, want)
		}
	}
}
