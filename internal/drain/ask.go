package drain

import (
	"time"

	"bd-auto/internal/ask"
	"bd-auto/internal/runner"
	"bd-auto/internal/runstate"
)

// The engine's half of the ask channel is three joins the ask package cannot
// make for itself, and nothing else.
//
// A broker knows how to hold a question and how to hand back an answer. It does
// not know which issue is asking, where a watcher is listening, or where an
// answer has to be written so that a re-run does not ask again. Those are all
// facts about a run, so they are wired here — which is also what keeps the
// engine free of any of it: attachAsk is one condition and one append, and a
// run with no broker takes exactly the path it took before this existed.

// WireAsk gives a broker the two things a run knows and it does not: where to
// raise a question so a watcher sees it, and where to write an answer down so a
// resumed run does not ask it twice.
//
// Both are optional. A nil bus means nothing is watching, which is normal for
// `bd-auto issue run`; an empty repoRoot means nothing is remembered, which is
// what a test that does not care gets.
func WireAsk(b *ask.Broker, bus *Bus, repoRoot string) {
	if b == nil {
		return
	}
	if bus != nil {
		b.Raise = func(q ask.Question) {
			bus.Emit(Event{
				Kind: EventQuestion, Issue: q.Issue, Role: runner.Role(q.Role),
				Text: q.Text, Question: &q,
			})
		}
		b.Settle = func(q ask.Question, a ask.Answer) {
			bus.Emit(Event{
				Kind: EventAnswer, Issue: q.Issue, Role: runner.Role(q.Role),
				Text: a.Text, Question: &q, Answer: &a,
			})
		}
	}
	if repoRoot != "" {
		b.Recall = func(q ask.Question) (ask.Answer, bool) { return recallAnswer(repoRoot, q) }
		b.Record = func(q ask.Question, a ask.Answer) { recordAnswer(repoRoot, q, a) }
	}
}

// recallAnswer reads back what a human already told this issue.
//
// Run state rather than the issue's notes, for the same reason a carried
// failure lives there: beads' post-checkout hook imports .beads/issues.jsonl
// over its database, so creating the next attempt's worktree reverts every bd
// write after that export. An answer written to the issue could be gone before
// the worker that needs it runs. .beads/auto/run.json is bd-auto's
// own file and nothing imports over it.
func recallAnswer(repoRoot string, q ask.Question) (ask.Answer, bool) {
	st, err := runstate.Load(repoRoot)
	if err != nil {
		return ask.Answer{}, false
	}
	prev, ok := st.AnswerFor(q.Issue, q.Text)
	if !ok {
		return ask.Answer{}, false
	}
	return ask.Answer{Text: prev.Answer, Source: ask.Source(prev.Source)}, true
}

func recordAnswer(repoRoot string, q ask.Question, a ask.Answer) {
	_, _ = runstate.Update(repoRoot, true, func(s *runstate.State) error {
		s.RecordQuestion(runstate.Question{
			ID: q.ID, Issue: q.Issue, Role: q.Role,
			Question: q.Text, Options: q.Labels(),
			Answer: a.Text, Source: string(a.Source),
			AskedAt: q.AskedAt.UTC(), AnsweredAt: time.Now().UTC(),
		})
		s.Note("%s asked, and was told: %s", q.Issue, firstLine(a.Text))
		return nil
	})
}

// attachAsk offers a run the ask_user tool.
//
// Three conditions, and each rules out a different way of getting this wrong: a
// run with no broker has nobody to answer, a backend without Tools cannot carry
// the offer at all, and a role the config did not name must not get it — the
// reviewer being the case that matters, since a reviewer that can question the
// author of the work it is judging is no longer an independent check.
func (e *Engine) attachAsk(req *runner.Request, in invocation) {
	if e.Ask == nil || in.Runner == nil || !in.Runner.Caps().Tools {
		return
	}
	if e.Cfg == nil || !e.Cfg.AskRole(string(in.Role)) {
		return
	}
	req.ToolServers = append(req.ToolServers, e.Ask.Spec(in.Issue, string(in.Role)))
}

// cancelAsk drops the questions an issue left open.
//
// Between two polls there is nobody parked on a question, so a worker dying is
// not something the broker can notice for itself. Without this a killed worker
// leaves its question on the screen until the timeout, asking a human to decide
// something for a process that no longer exists.
func (e *Engine) cancelAsk(issue string) {
	if e.Ask == nil {
		return
	}
	if n := e.Ask.Broker().CancelIssue(issue); n > 0 {
		e.logf("%s: dropped %d unanswered question(s); the worker has stopped", issue, n)
	}
}
