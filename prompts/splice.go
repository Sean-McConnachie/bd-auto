package prompts

import (
	_ "embed"
	"regexp"
	"strings"
)

// The splices a prompt may place inside itself.
//
// They are a documented surface rather than an internal detail, because the
// prompt a role runs under can now come from a file in the repo
// (.beads-auto/agents/<role>.md) instead of from this package. A repo that
// wants to add to the shipped reviewer rather than replace it writes
// {{BUILTIN}} and its own paragraphs underneath; one that wants the verdict
// contract somewhere other than the end says where with {{VERDICT}}.
//
// A splice with nothing to splice yields nothing. Not an error and not the
// literal token: a prompt that mentions a code index in a repo that has none is
// worse than a prompt that never mentions one.
const (
	// TokenBuiltin expands to the shipped prompt for the role of this name, and
	// to nothing for a name this package has no prompt for.
	TokenBuiltin = "{{BUILTIN}}"
	// TokenGraph expands to the code-index section, and to nothing where there
	// is no index.
	TokenGraph = "{{GRAPH}}"
	// TokenVerdict expands to the verdict contract for a judging stage, and to
	// nothing for a role that is not judging anything.
	TokenVerdict = "{{VERDICT}}"
)

//go:embed verdict.md
var verdict string

// VerdictContract is what a judging stage's final message must look like, in
// the words the engine's parser is written against.
//
// It lives here, in one file, because it is a contract rather than advice:
// drain.ParseVerdict reads the VERDICT: line literally and treats its absence
// as a failure, so a judging prompt that does not carry this text fails every
// issue it ever sees. Every judging prompt gets it — at {{VERDICT}} where the
// author said so, appended where they did not.
func VerdictContract() string { return strings.TrimRight(verdict, "\n") }

// Splices is what each token expands to for one resolution. An empty field
// means the token disappears.
type Splices struct {
	Builtin string
	Graph   string
	Verdict string
}

// blankRun is three or more newlines: what is left behind when a token that
// stood alone in its own paragraph expands to nothing.
var blankRun = regexp.MustCompile(`\n{3,}`)

// Splice substitutes the tokens in text and tidies up after the ones that
// expanded to nothing, so a prompt reads the same whether or not its splices
// had anything in them.
//
// {{BUILTIN}} goes first and on its own, because what it brings in has splices
// of its own: the shipped reviewer places {{VERDICT}}. One pass over the whole
// text would leave that token behind as a literal, since a replacement is not
// rescanned.
func Splice(text string, s Splices) string {
	out := strings.ReplaceAll(text, TokenBuiltin, strings.TrimRight(s.Builtin, "\n"))
	r := strings.NewReplacer(
		// A {{BUILTIN}} inside a built-in does not recurse; it disappears.
		TokenBuiltin, "",
		TokenGraph, strings.TrimRight(s.Graph, "\n"),
		TokenVerdict, strings.TrimRight(s.Verdict, "\n"),
	)
	out = r.Replace(out)
	out = blankRun.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out) + "\n"
}

// HasToken reports whether text places a splice itself. It is how a caller
// tells "the author put the verdict contract where they wanted it" apart from
// "the author never mentioned it", which are handled differently: the second
// gets it appended.
func HasToken(text, token string) bool { return strings.Contains(text, token) }
