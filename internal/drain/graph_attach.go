package drain

import (
	"strings"

	"bd-auto/internal/graph"
	"bd-auto/internal/runner"
	"bd-auto/prompts"
)

// graphTools is what a role needs to run the four commands the prompt names.
//
// Scoped to the binary rather than to Bash, for the same reason the reviewer's
// list spells out `Bash(git diff:*)` instead of `Bash`: an allowlist entry is
// the whole of what a scoped run may do, and a bare Bash entry here would hand
// the reviewer a shell in exchange for a symbol lookup.
var graphTools = []string{"Bash(graphify:*)"}

// attachGraph tells a role about the code index, when there is one to tell it
// about.
//
// Four conditions, and each rules out a different way of wasting tokens. The
// config may not want an index at all; it may not want this role to have one;
// there may be no graphify on this machine; and the build may have failed. Any
// of them means the role prompt says nothing about an index — a model told about
// a tool that is not there will try it, read the error, and try something else,
// which costs more than never mentioning it.
//
// The index is read from disk on each invocation rather than cached on the
// engine, because a wave barrier refreshes it between waves and a cached Index
// would name a stamp that is no longer the one on disk.
func (e *Engine) attachGraph(req *runner.Request, in invocation) {
	if e.Cfg == nil || !e.Cfg.IndexFor(string(in.Role)) {
		return
	}
	idx := graph.Read(e.RepoRoot)
	if !idx.Built {
		return
	}
	section := prompts.Graph(idx.Path)
	req.SystemPrompt = strings.TrimRight(req.SystemPrompt, "\n") + "\n\n" + section
	req.AllowedTools = append(req.AllowedTools, graphTools...)
}
