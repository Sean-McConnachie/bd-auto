package cmds

import (
	"strings"
	"testing"

	"bd-auto/internal/runstate"
)

func state(autonomy string) *runstate.State {
	s := runstate.New("epic-1", 5, autonomy, 1)
	s.Wave = 2
	return s
}

// The Stop hook is the autonomy engine. These cases pin down exactly when the
// session is allowed to stop, because getting this wrong either wedges the user
// in a loop or lets a run quietly abandon itself half-finished.
func TestStopDecision(t *testing.T) {
	cases := []struct {
		name      string
		autonomy  string
		inFlight  []string
		ready     []string
		unmerged  []string
		wantBlock bool
		wantText  string
	}{
		{
			name:     "nothing left anywhere lets the session stop",
			autonomy: "auto", wantBlock: false,
		},
		{
			name:     "workers still running blocks",
			autonomy: "auto", inFlight: []string{"i-1", "i-2"},
			wantBlock: true, wantText: "Still in flight",
		},
		{
			name: "unmerged branches block even with ready work",
			// Critical: starting a new wave here would branch worktrees from a
			// HEAD that does not contain the finished wave.
			autonomy: "auto", ready: []string{"i-9"}, unmerged: []string{"bd-auto/i-1"},
			wantBlock: true, wantText: "unmerged",
		},
		{
			name:     "ready work blocks in auto mode",
			autonomy: "auto", ready: []string{"i-3"},
			wantBlock: true, wantText: "plan --dispatch",
		},
		{
			name:     "ready work does NOT block in wave mode: that is the pause point",
			autonomy: "wave", ready: []string{"i-3"},
			wantBlock: false,
		},
		{
			name:     "ready work does NOT block in issue mode",
			autonomy: "issue", ready: []string{"i-3"},
			wantBlock: false,
		},
		{
			name:     "wave mode still blocks while workers are running",
			autonomy: "wave", inFlight: []string{"i-1"},
			wantBlock: true, wantText: "Still in flight",
		},
		{
			name:     "wave mode still blocks on unmerged branches",
			autonomy: "wave", unmerged: []string{"bd-auto/i-1"},
			wantBlock: true, wantText: "integrator",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, block := stopDecision(state(tc.autonomy), tc.inFlight, tc.ready, tc.unmerged)
			if block != tc.wantBlock {
				t.Fatalf("block = %v, want %v (msg: %q)", block, tc.wantBlock, msg)
			}
			if tc.wantText != "" && !strings.Contains(msg, tc.wantText) {
				t.Fatalf("message should mention %q, got:\n%s", tc.wantText, msg)
			}
			if !block && msg != "" {
				t.Fatalf("no block should mean no message, got %q", msg)
			}
		})
	}
}

func TestStopDecisionAlwaysExplainsItself(t *testing.T) {
	msg, block := stopDecision(state("auto"), []string{"i-1"}, nil, nil)
	if !block {
		t.Fatal("expected a block")
	}
	for _, want := range []string{"epic-1", "wave 2", "Do not stop"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("block message must include %q so the model can act on it, got:\n%s", want, msg)
		}
	}
}

func TestFooterRegex(t *testing.T) {
	cases := []struct{ in, issue, branch, status string }{
		{"work done\n\nBD-AUTO: issue=bd-42 branch=bd-auto/bd-42 status=done", "bd-42", "bd-auto/bd-42", "done"},
		{"BD-AUTO:  issue=x-1   branch=b/x-1  status=blocked", "x-1", "b/x-1", "blocked"},
	}
	for _, tc := range cases {
		m := footerRe.FindStringSubmatch(tc.in)
		if m == nil {
			t.Fatalf("footer not matched in %q", tc.in)
		}
		if m[1] != tc.issue || m[2] != tc.branch || m[3] != tc.status {
			t.Fatalf("got %v, want %s/%s/%s", m[1:], tc.issue, tc.branch, tc.status)
		}
	}
	if footerRe.MatchString("I finished the task, all done") {
		t.Fatal("prose must not be mistaken for a report footer")
	}
}

func TestClaimRegex(t *testing.T) {
	shouldMatch := map[string]string{
		"bd update bd-42 --claim":           "bd-42",
		"bd update beads-x-eqc.3 --claim":   "beads-x-eqc.3",
		"cd /tmp && bd update bd-7 --claim": "bd-7",
		"bd --actor=w1 update bd-9 --claim": "bd-9",
	}
	for cmd, want := range shouldMatch {
		m := claimRe.FindStringSubmatch(cmd)
		if m == nil {
			t.Fatalf("claim not detected in %q", cmd)
		}
		if m[1] != want {
			t.Fatalf("%q: got issue %q, want %q", cmd, m[1], want)
		}
	}
	for _, cmd := range []string{"bd show bd-42", "echo bd update --claim-not"} {
		if claimRe.MatchString(cmd) && !strings.Contains(cmd, "--claim") {
			t.Fatalf("false positive on %q", cmd)
		}
	}
}

// Workers must never integrate: only the integrator merges, at the barrier.
func TestForbiddenCommandsForWorkers(t *testing.T) {
	blocked := []string{
		"git merge main",
		"git rebase origin/main",
		"git push -u origin HEAD",
		"git cherry-pick abc123",
		"cd /x && git merge main",
		"git status; git push",
		"git status || git push",
		"(git push)",
		// git can be reached without being the first word. An earlier fix
		// anchored the match to command position and let every one of these
		// through, which is a worker able to integrate.
		"sudo git push",
		"env FOO=1 git push",
		"nohup git push origin HEAD",
		"for b in x; do git push; done",
		"if ! git merge main; then echo no; fi",
		// Quoted, but the quotes are the payload of an executor rather than
		// data. Stripping them would hide exactly what we look for.
		`eval "git push"`,
		"bash -c 'git push'",
		`sh -c "git merge main"`,
		`xargs -I{} git push {}`,
		// An apostrophe in a trailing comment leaves the scan inside a quote,
		// which blanks the rest of the input — including the next line, which
		// the shell still runs.
		"git status # it's fine\ngit push",
	}
	for _, c := range blocked {
		if !forbidsIntegration(c) {
			t.Fatalf("%q should be blocked for workers", c)
		}
	}
	allowed := []string{
		"git commit -m 'work'",
		"git switch -c bd-auto/bd-1",
		"git status",
		"git diff HEAD",
		"git add -A",
		"git log --oneline -5",
		// Found live: a worker recording what the guard did could not, because
		// the guard matched the example inside its own quoted note.
		`bd update bd-1 --append-notes="the guard denied 'git merge main'"`,
		`echo "run git push yourself"`,
		"git commit -m 'do not git push this'",
		`bd update bd-1 --append-notes="a \"git push\" inside escaped quotes"`,
	}
	for _, c := range allowed {
		if forbidsIntegration(c) {
			t.Fatalf("%q is normal worker activity and must not be blocked", c)
		}
	}
}

// Stripping must not glue neighbouring words together, or `foo'x'git push`
// would read as one token and slip past the guard. It must also report when it
// ran off the end inside a quote, because everything after an unterminated
// quote is dropped and the caller has to fall back to the raw command.
func TestStripQuotedLeavesRunnableWords(t *testing.T) {
	cases := []struct {
		in       string
		want     []string
		balanced bool
	}{
		{`git commit -m 'work'`, []string{"git", "commit", "-m"}, true},
		{`echo "a" git push`, []string{"echo", "git", "push"}, true},
		{`a\'b git push`, []string{"a", "b", "git", "push"}, true},
		{`bd update x --append-notes="it's done"`, []string{"bd", "update", "x", "--append-notes="}, true},
		// The dropped text is why this must report false: on its own the
		// result looks harmless.
		{"echo 'unterminated git push", []string{"echo"}, false},
		{"git status # it's fine\ngit push", []string{"git", "status", "#", "it"}, false},
	}
	for _, tc := range cases {
		stripped, balanced := stripQuoted(tc.in)
		if balanced != tc.balanced {
			t.Fatalf("stripQuoted(%q) balanced = %v, want %v", tc.in, balanced, tc.balanced)
		}
		got := strings.Fields(stripped)
		if len(got) != len(tc.want) {
			t.Fatalf("stripQuoted(%q) tokenised to %q, want %q", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("stripQuoted(%q) tokenised to %q, want %q", tc.in, got, tc.want)
			}
		}
	}
}
