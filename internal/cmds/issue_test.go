package cmds

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bd-auto/internal/bd"
	"bd-auto/internal/runner"
	"bd-auto/internal/runner/fake"
)

// TestIssueRunEndToEnd drives the real command over a real repo: config from
// disk, adapters from the registry, worktree and guards from git. Only the two
// things that would cost money or need a database are substituted — the model,
// by the fake provider, and bd, by a stub on PATH.
//
// It cannot run in parallel with anything else: fake.Install replaces a
// process-wide registry entry, and the command reads the working directory.
func TestIssueRunEndToEnd(t *testing.T) {
	repo := cmdTestRepo(t)
	statusFile := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(statusFile, []byte("open\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubBD(t, statusFile)

	writeFile(t, filepath.Join(repo, ".beads-auto.yaml"), `
gate:
  - name: build
    run: "true"
pipeline:
  - stage: implement
  - stage: gate
  - stage: review
    agent: reviewer
max_rounds: 2
retry: 0
runners:
  default:
    provider: fake
`)

	// One shared script: the worker runs first, the reviewer second.
	r := fake.New(
		fake.Step{Text: "done", Do: func(_ context.Context, req runner.Request) error {
			if err := os.WriteFile(filepath.Join(req.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
				return err
			}
			if _, err := gitOut(req.Dir, "add", "-A"); err != nil {
				return err
			}
			if _, err := gitOut(req.Dir, "commit", "--quiet", "-m", "work"); err != nil {
				return err
			}
			return os.WriteFile(statusFile, []byte("closed\n"), 0o644)
		}},
		fake.Step{Text: "VERDICT: pass"},
	)
	r.Repeat = false
	defer fake.Install(r)()

	t.Setenv("BD_AUTO_REPO", repo)
	t.Chdir(repo)

	if err := Issue([]string{"run", "--issue", "t-1", "--quiet"}); err != nil {
		t.Fatalf("issue run: %v", err)
	}
	if r.Calls() != 2 {
		t.Fatalf("%d model calls, want one worker and one reviewer", r.Calls())
	}
	reqs := r.Requests()
	if reqs[0].Role != runner.RoleWorker || reqs[1].Role != runner.RoleReviewer {
		t.Fatalf("roles %s then %s", reqs[0].Role, reqs[1].Role)
	}
	// The role prompts are what override the repo's own CLAUDE.md, so a run
	// that forgets them is a run whose worker has been told to push.
	if !strings.Contains(reqs[0].SystemPrompt, "bd-auto worker") {
		t.Fatalf("the worker got no role prompt:\n%s", reqs[0].SystemPrompt)
	}
	if !strings.Contains(reqs[1].SystemPrompt, "bd-auto reviewer") {
		t.Fatalf("the reviewer got no role prompt:\n%s", reqs[1].SystemPrompt)
	}
	if !exists(filepath.Join(repo, ".beads", "auto", "review", "t-1.md")) {
		t.Fatal("no review notes were written")
	}
}

// --dangerously-skip-permissions has to reach every model the run spawns, and
// the reviewer is the one that proves it: it is the only role that ships with a
// permission level of its own, so a flag that resolved underneath the config
// would leave it scoped.
//
// This drives the real command rather than the resolver, because the bug the
// flag exists for was never in the resolver — the plumbing under it already
// worked, and what was missing was any way to reach it from a command line.
func TestSkipPermissionsReachesEveryRole(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantWorker   runner.Permissions
		wantReviewer runner.Permissions
	}{
		{
			name:         "without the flag each role keeps its own level",
			args:         []string{"run", "--issue", "t-1", "--quiet"},
			wantWorker:   runner.PermAuto,
			wantReviewer: runner.PermScoped,
		},
		{
			name:         "with it nothing is left behind",
			args:         []string{"run", "--issue", "t-1", "--quiet", "--dangerously-skip-permissions"},
			wantWorker:   runner.PermBypass,
			wantReviewer: runner.PermBypass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := cmdTestRepo(t)
			statusFile := filepath.Join(t.TempDir(), "status")
			writeFile(t, statusFile, "open\n")
			stubBD(t, statusFile)

			// Both roles name a level, so neither can pass by inheriting one.
			writeFile(t, filepath.Join(repo, ".beads-auto.yaml"), `
pipeline:
  - stage: implement
  - stage: review
    agent: reviewer
max_rounds: 2
retry: 0
runners:
  default:
    provider: fake
    permissions: auto
  reviewer:
    permissions: scoped
    allowed_tools: [Read]
`)

			r := fake.New(
				fake.Step{Text: "done", Do: func(_ context.Context, req runner.Request) error {
					if err := os.WriteFile(filepath.Join(req.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
						return err
					}
					if _, err := gitOut(req.Dir, "add", "-A"); err != nil {
						return err
					}
					if _, err := gitOut(req.Dir, "commit", "--quiet", "-m", "work"); err != nil {
						return err
					}
					return os.WriteFile(statusFile, []byte("closed\n"), 0o644)
				}},
				fake.Step{Text: "VERDICT: pass"},
			)
			r.Repeat = false
			defer fake.Install(r)()

			t.Setenv("BD_AUTO_REPO", repo)
			t.Chdir(repo)

			if err := Issue(tc.args); err != nil {
				t.Fatalf("issue run: %v", err)
			}
			reqs := r.Requests()
			if len(reqs) != 2 {
				t.Fatalf("%d model calls, want one worker and one reviewer", len(reqs))
			}
			if got := reqs[0].Permissions; got != tc.wantWorker {
				t.Errorf("worker permissions = %q, want %q", got, tc.wantWorker)
			}
			if got := reqs[1].Permissions; got != tc.wantReviewer {
				t.Errorf("reviewer permissions = %q, want %q", got, tc.wantReviewer)
			}
		})
	}
}

func TestIssueRejectsAnUnknownSubcommand(t *testing.T) {
	if err := Issue([]string{"drain"}); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	if err := Issue(nil); err == nil {
		t.Fatal("a missing subcommand was accepted")
	}
}

// --- harness ---

func cmdTestRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main", "."},
		{"config", "user.name", "bd-auto test"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := gitOut(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	if _, err := gitOut(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOut(dir, "commit", "--quiet", "-m", "seed"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubBD puts a bd on PATH that answers `show` from a file. The engine only
// reads bd on the happy path, so this is the whole surface it needs.
func stubBD(t *testing.T, statusFile string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bd-stub")
	script := "#!/usr/bin/env sh\n" +
		"if [ \"$1\" = \"show\" ]; then\n" +
		"  printf '{\"id\":\"t-1\",\"title\":\"stub issue\",\"status\":\"%s\"}\\n' \"$(cat " + statusFile + ")\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := bd.Binary
	bd.Binary = path
	t.Cleanup(func() { bd.Binary = prev })
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
