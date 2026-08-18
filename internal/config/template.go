package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrConfigExists is returned by Write when a config file is already present and
// the caller did not ask to replace it. Clobbering a config someone tuned by
// hand is not something to do on a guess.
var ErrConfigExists = errors.New("config file already exists")

// Template returns the contents of a starter .beads-auto.yaml.
//
// The values are interpolated from the Default* constants rather than written
// out by hand, so the file this generates cannot drift from the defaults the
// loader actually applies.
func Template() []byte {
	d := Default()
	return []byte(fmt.Sprintf(`# bd-auto configuration for this repo.
#
# Every field has a default, so this file is optional and every entry below can
# be deleted. `+"`bd-auto config show`"+` prints the resolved values.

# Gate commands. All must exit 0 for an issue to pass. They run inside the
# worker's worktree, and again on the merged result at the wave barrier.
#
# With no gate configured the gate stage passes trivially, which is what lets
# bd-auto be used in a repo that has no test suite yet. Uncomment and edit:
# gate:
#   - name: build
#     run: make build
#   - name: test
#     run: make test

# The per-issue pipeline, in order.
#   stage: implement  - built in, the worker itself
#   stage: gate       - built in, runs the gate commands above
#   agent: <role>     - a model, run with the named role's runner config
#   run: <command>    - executed by bd-auto; must exit 0
#
# agent: names a runner role: worker, reviewer, integrator, or any key you add
# under runners: below. A name that is not a defined role fails at load.
#
# Add your own stages here. A custom review pipeline is just another entry:
#   - stage: security
#     run: ./scripts/security-review.sh
#     optional: true
# run: stages get $BD_ISSUE, $BD_BRANCH, $BD_WORKTREE, $BD_REPO_ROOT and
# $BD_DIFF_FILE in their environment.
pipeline:
  - stage: implement
  - stage: gate
  - stage: review
    agent: reviewer
    # A stage may cap its own feedback rounds; unset means max_rounds below.
    # max_rounds: %d

# How the model is run, per role. Every role resolves over "default", so an
# entry only names what it changes. The values below are the built-in ones.
#
# permissions defaults to auto, and widening it is your call rather than ours.
# Worth knowing before the first run: headless there is nobody to answer a
# permission prompt, so under auto a worker is refused every write and every
# shell command, and under acceptEdits it still cannot run the gate, git or bd.
# Only bypass lets a worker finish. Set it here to mean it for this repo, or
# pass --dangerously-skip-permissions to mean it for one run. What keeps a
# worker in bounds either way is structural: a throwaway worktree, its own
# branch, hooks that refuse push, merge and rebase, and a scope you confirmed
# before anything was spawned. A run that does hit the refusal stops and says
# so, naming the tools and the flag, rather than parking the issue.
# runners:
#   default:
#     provider: claude
#     model: opus
#     permissions: auto      # scoped | auto | bypass
#     timeout: 0             # seconds; 0 = unlimited, and unlimited is the point
#   reviewer:
#     model: sonnet
#     permissions: scoped
#     resume: false          # a reviewer judges the diff fresh each time
#     # denied_tools defaults to every bd verb that writes the record. Deny
#     # rules are checked ahead of the permission level, so they are what keeps
#     # a reviewer out of issue state even under bypass. Setting this replaces
#     # the built-in list rather than adding to it.
#   integrator:
#     model: opus

# Feedback rounds within one attempt: how many times a failed gate, review or
# guard check may send work back to the same worker before the attempt fails.
# This is the cheap recovery. Measured against the same work done by a fresh
# worker instead, sending it back cost 18%% less and finished 26%% sooner,
# because a fresh worker re-runs its exploration and then re-sends every result
# of it for the rest of the attempt.
max_rounds: %d

# Workers per wave. The DAG decides how many issues are genuinely independent;
# this caps how many of them run at once.
concurrency: %d

# auto  - drain the selected scope without stopping
# wave  - pause at each wave barrier and wait for `+"`bd-auto run resume`"+`
autonomy: %s

# Extra attempts after the first failure. 1 means "retry once fresh, then park".
# The safety net, not the main path: a fresh attempt throws away the worktree
# and the session, so it is what to reach for when the session itself has gone
# wrong, and max_rounds above is what to reach for otherwise.
retry: %d

# What a wave barrier does with what its workers found.
#   triage    stage it in .beads/auto/triage.json and file nothing; the command
#             "bd-auto triage" is what turns one into an issue. The default:
#             filing is the irreversible half, and a backlog grows whether or
#             not a run learned anything.
#   defer     file it as an issue, hidden from "bd ready" until a human wants it
#   immediate file it and offer it to the next run
discovered_work: %s

# Prepended to the issue ID to form a worker branch. Must end with /.
branch_prefix: %s

# Where a finished run ends up. Both switches are on, so by default a run
# publishes nothing on its own: every issue branch is merged, in dependency
# order, onto one temporary epic branch, and the branch you work on is never
# written to. Once the whole run has landed clean and the gate is green on the
# merged result, the branch is pushed and a pull request opens against the
# branch the run started from. A parked issue or a red gate opens nothing and
# leaves the epic branch in place for you to look at.
#
# The two are separate switches. pr: false still produces the epic branch and
# leaves it alone, which is what a repo with no remote or no gh wants.
# branch: false is the escape hatch back to merging straight into your own
# branch, and it turns the pull request off with it: there would be nothing to
# open one from.
handoff:
  branch: %v
  pr: %v
  # The remote the epic branch is pushed to.
  remote: %s
  # Prepended to the epic's ID to form the branch. Must end with /.
  prefix: %s

# The ask_user tool: a worker that hits a genuine ambiguity can put a question
# to the human watching the run and get an answer back, without ending its
# session to do it.
#
# It only ever reaches somebody when there is a live view to reach — a run with
# --quiet, --plain, --json or no terminal answers on the spot with "nobody is
# watching, decide for yourself and write down what you assumed", so an
# unattended drain never stalls on a question.
ask:
  enabled: %v
  # How long a question waits for an answer, in seconds. 0 waits forever, which
  # only makes sense where somebody is always watching. The wait costs one idle
  # worker; the rest of the wave carries on.
  timeout: %d
  # How long one tool call blocks before handing the model a ticket to poll
  # with, in seconds. This is the number that has to fit inside the backend's
  # own limit on a single tool call — Claude Code kills an idle stdio call after
  # thirty minutes — and the ticket is what lets a question outlive it. Lower it
  # for a stricter backend.
  hold: %d
  # Which roles may ask. The reviewer is deliberately absent: it is read-only
  # and judging somebody else's work, and a reviewer that can question the
  # author is no longer an independent check.
  roles: [%s]
`, DefaultMaxRounds, d.MaxRounds, d.Concurrency, d.Autonomy, d.Retry,
		d.DiscoveredWork, d.BranchPrefix,
		d.StageOnBranch(), d.OpenPR(), d.HandoffRemote(), d.EpicBranchPrefix(),
		d.AskEnabled(), DefaultAskTimeout, DefaultAskHold, strings.Join(DefaultAskRoles(), ", ")))
}

// Write creates a starter config file in dir and reports the path it wrote.
//
// Without force an existing file is left alone and ErrConfigExists is returned,
// so callers can tell "already set up" apart from a real failure.
func Write(dir string, force bool) (string, error) {
	p := filepath.Join(dir, FileName)
	if !force {
		if _, err := os.Stat(p); err == nil {
			return p, ErrConfigExists
		} else if !os.IsNotExist(err) {
			return p, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	if err := os.WriteFile(p, Template(), 0o644); err != nil {
		return p, fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}
