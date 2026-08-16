package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
#   integrator:
#     model: opus

# Feedback rounds within one attempt: how many times a failed gate, review or
# guard check may send work back to the same worker before the attempt fails.
max_rounds: %d

# Workers per wave. The DAG decides how many issues are genuinely independent;
# this caps how many of them run at once.
concurrency: %d

# auto  - drain the epic without stopping
# wave  - pause at each wave barrier
# issue - pause after every issue
autonomy: %s

# Extra attempts after the first failure. 1 means "retry once fresh, then park".
retry: %d

# Where discovered work goes. "defer" keeps it out of the current run.
discovered_work: %s

# Prepended to the issue ID to form a worker branch. Must end with /.
branch_prefix: %s

# Caps a worker's report back to the orchestrator, protecting its context.
report_max_lines: %d
`, DefaultMaxRounds, d.MaxRounds, d.Concurrency, d.Autonomy, d.Retry,
		d.DiscoveredWork, d.BranchPrefix, d.ReportMaxLines))
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
