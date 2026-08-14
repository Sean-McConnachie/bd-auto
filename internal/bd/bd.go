// Package bd wraps the bd (beads) CLI.
//
// bd is the source of truth for issues, dependencies and readiness. This
// package deliberately does not cache or reimplement any of that: bd ready is
// already blocker-aware and bd ready --claim is already atomic, and the
// concurrency spike (eqc.1) confirmed both hold under five concurrent workers.
package bd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Binary is the bd executable name, resolved from PATH.
var Binary = "bd"

// HumanLabel marks an issue as needing a human. bd human list finds issues by
// this label.
const HumanLabel = "human"

// Ref is a lightweight issue reference as it appears in dependency lists.
type Ref struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Type   string `json:"dependency_type,omitempty"`
}

// Issue is a beads issue. Fields absent from a given bd command's output stay
// zero.
type Issue struct {
	ID                 string    `json:"id"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	Design             string    `json:"design"`
	AcceptanceCriteria string    `json:"acceptance_criteria"`
	Notes              string    `json:"notes"`
	Status             string    `json:"status"`
	Priority           int       `json:"priority"`
	IssueType          string    `json:"issue_type"`
	Assignee           string    `json:"assignee"`
	Labels             []string  `json:"labels"`
	Parent             string    `json:"parent"`
	Dependencies       []Ref     `json:"dependencies"`
	Dependents         []Ref     `json:"dependents"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Closed reports whether the issue reached a closed state.
func (i Issue) Closed() bool { return i.Status == "closed" }

// Blocked reports whether the issue is parked as blocked.
func (i Issue) Blocked() bool { return i.Status == "blocked" }

// Terminal reports whether an issue is in a state a worker may stop on.
func (i Issue) Terminal() bool { return i.Closed() || i.Blocked() }

// HasLabel reports whether the issue carries a label.
func (i Issue) HasLabel(l string) bool {
	for _, x := range i.Labels {
		if x == l {
			return true
		}
	}
	return false
}

// Client runs bd against a repository.
type Client struct {
	// Dir is the directory bd runs in. bd resolves the main repo's .beads/
	// from inside a git worktree, so a worktree path works here too.
	Dir string
	// Timeout bounds any single bd invocation.
	Timeout time.Duration
}

// New returns a client rooted at dir.
func New(dir string) *Client {
	return &Client{Dir: dir, Timeout: 120 * time.Second}
}

// Run executes bd with args and returns stdout. Stderr is folded into the
// error, since bd writes advisory noise there.
func (c *Client) Run(args ...string) ([]byte, error) {
	cmd := exec.Command(Binary, args...)
	cmd.Dir = c.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("bd %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runJSON executes bd and decodes JSON output into v.
func (c *Client) runJSON(v any, args ...string) error {
	out, err := c.Run(append(args, "--json")...)
	if err != nil {
		return err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("bd %s: decode json: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Ready returns issues with no active blockers under parent, in bd's priority
// order. An empty parent queries the whole repo.
func (c *Client) Ready(parent string, limit int) ([]Issue, error) {
	args := []string{"ready"}
	if parent != "" {
		args = append(args, "--parent", parent)
	}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprint(limit))
	}
	var out []Issue
	if err := c.runJSON(&out, args...); err != nil {
		return nil, err
	}
	return out, nil
}

// Show returns one issue with its full text fields and dependency lists.
func (c *Client) Show(id string) (*Issue, error) {
	var raw json.RawMessage
	if err := c.runJSON(&raw, "show", id); err != nil {
		return nil, err
	}
	// bd show returns either an object or a single-element array depending on
	// the code path; accept both.
	var one Issue
	if err := json.Unmarshal(raw, &one); err == nil && one.ID != "" {
		return &one, nil
	}
	var many []Issue
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		return &many[0], nil
	}
	return nil, fmt.Errorf("bd show %s: no issue in output", id)
}

// Children returns every issue under parent, closed ones included.
func (c *Client) Children(parent string) ([]Issue, error) {
	var out []Issue
	if err := c.runJSON(&out, "list", "--parent", parent, "--all", "--limit", "0", "--flat"); err != nil {
		return nil, err
	}
	return out, nil
}

// SwarmValidate runs bd's own DAG check for an epic and returns its report.
// Warnings here (wrong dependency direction, orphans, cycles) mean a run would
// mis-order work, so the orchestrator surfaces them rather than pressing on.
func (c *Client) SwarmValidate(epic string) (string, error) {
	out, err := c.Run("swarm", "validate", epic)
	return string(out), err
}

// Claim atomically claims a specific issue for the current actor.
func (c *Client) Claim(id string) error {
	_, err := c.Run("update", id, "--claim")
	return err
}

// Close closes an issue with a reason.
func (c *Client) Close(id, reason string) error {
	args := []string{"close", id}
	if reason != "" {
		args = append(args, "--reason="+reason)
	}
	_, err := c.Run(args...)
	return err
}

// Reopen reopens a closed issue.
func (c *Client) Reopen(id string) error {
	_, err := c.Run("reopen", id)
	return err
}

// AppendNotes appends to an issue's notes.
//
// Caller beware: bd's notes field is a read-modify-write with no locking. The
// spike showed concurrent appends to the SAME issue silently lose data and
// still exit 0. Only ever call this for an issue whose worker has finished.
func (c *Client) AppendNotes(id, note string) error {
	_, err := c.Run("update", id, "--append-notes="+note)
	return err
}

// Park marks an issue blocked, records why, and flags it for a human.
func (c *Client) Park(id, reason string) error {
	if err := c.AppendNotes(id, reason); err != nil {
		return err
	}
	_, err := c.Run("update", id, "--status=blocked", "--add-label="+HumanLabel)
	return err
}

// Stats is a small summary used by run status.
type Stats struct {
	Total   int
	Open    int
	Closed  int
	Blocked int
}

// EpicStats counts the children of an epic by status.
func (c *Client) EpicStats(epic string) (Stats, error) {
	kids, err := c.Children(epic)
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	for _, k := range kids {
		if k.ID == epic {
			continue
		}
		s.Total++
		switch k.Status {
		case "closed":
			s.Closed++
		case "blocked":
			s.Blocked++
		default:
			s.Open++
		}
	}
	return s, nil
}

// RepoRoot returns the main checkout's root, resolved correctly from inside a
// git worktree. Every worker shares the main repo's run state, so this must not
// return the worktree path.
func RepoRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repo root from %s: %w", dir, err)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("resolve repo root from %s: empty git-common-dir", dir)
	}
	return filepath.Dir(gitDir), nil
}
