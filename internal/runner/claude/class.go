package claude

import (
	"strings"

	"bd-auto/internal/runner"
)

// infraPatterns are the substrings that mean the run never got a fair chance:
// the account is rate limited or out of quota, the credentials are stale, the
// network or the API is down, or the CLI itself fell over before it could do
// any work.
//
// Getting one of these wrong in the safe direction costs a retry. Getting it
// wrong in the other direction is the failure this whole taxonomy exists to
// prevent: five parallel workers meeting one 429 each, burning every round and
// every attempt they have, and parking five perfectly good issues.
//
// Matching is case-insensitive substring, and only ever against a run that has
// already failed — otherwise a model that merely writes the word "429" in its
// report would classify its own success as an outage.
var infraPatterns = []string{
	// quota and rate limits
	"usage limit",
	"rate limit",
	"rate_limit_error",
	"429",
	"credit balance is too low",
	"insufficient_quota",
	// The plan limits the CLI enforces itself. It words each one differently
	// and none of them says "usage limit": measured on 2026-08-18, a drain met
	// "You've hit your session limit · resets 3:20pm (Europe/Berlin)" on five
	// workers and matched none of the patterns above. These stay narrow —
	// "limit" alone is a word a model writes about code — because apiOutage is
	// what actually catches these now, and this list is only the fallback for a
	// CLI too old to report terminal_reason.
	"session limit",
	"weekly limit",
	"hour limit",
	"quota exceeded",
	// upstream capacity
	"overloaded",
	"529",
	"api error: 500",
	"api error: 502",
	"api error: 503",
	"internal server error",
	"service unavailable",
	"bad gateway",
	// credentials
	"authentication_error",
	"invalid api key",
	"invalid_api_key",
	"oauth token has expired",
	"please run /login",
	"unauthorized",
	// network
	"econnreset",
	"econnrefused",
	"enotfound",
	"etimedout",
	"socket hang up",
	"connection refused",
	"network error",
	"fetch failed",
	"getaddrinfo",
	// the CLI itself
	"cannot find module",
	"javascript heap out of memory",
	"segmentation fault",
}

// infraSignal reports whether any of the texts names an infrastructure failure.
func infraSignal(texts ...string) bool {
	for _, t := range texts {
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		for _, pat := range infraPatterns {
			if strings.Contains(low, pat) {
				return true
			}
		}
	}
	return false
}

// terminalAPIError is the CLI's terminal_reason for a run that ended because a
// request to the API failed, as opposed to anything the model did.
const terminalAPIError = "api_error"

// apiOutage reports whether the CLI itself said the API call failed.
//
// This is the structured half of infra detection, and it is the half that is
// trustworthy. infraSignal reads prose, which is a losing game: the product
// invents new wording for every limit it adds, and each new phrase silently
// becomes a work failure until somebody notices five parked issues and a bill.
// terminal_reason and api_error_status are the CLI stating a fact about the
// request, so any API error is an outage here whatever its status — a 429, a
// 500 and a 400 are all "this request did not reach a model", which is the
// question this function answers. None of them is a verdict on the issue.
func apiOutage(status int, terminalReason string) bool {
	return status != 0 || strings.EqualFold(terminalReason, terminalAPIError)
}

// outcome is everything classification looks at. It is a struct so the mapping
// can be table-tested without a process.
type outcome struct {
	// ctxErr is the caller's context error: a TUI stop, or a kill.
	ctxErr error
	// timedOut is Request.Timeout firing, as opposed to the caller cancelling.
	timedOut bool
	// startErr is set when the process never ran at all.
	startErr error
	// exitCode is -1 when there is no exit status.
	exitCode int
	// sawResult is whether the CLI printed its final result line. Without one
	// the process did not reach a verdict, whatever it exited with.
	sawResult bool
	// resultErr is the result line reporting failure.
	resultErr bool
	// failText is what the result line said about the failure.
	failText string
	// stderr is the tail of the process's stderr.
	stderr string
	// apiStatus and terminalReason are the result line's own account of an
	// outage: api_error_status and terminal_reason. Zero and empty when the CLI
	// did not say, which reads as no API error.
	apiStatus      int
	terminalReason string
}

// classify maps an outcome onto the class the engine branches on.
//
// The order matters more than any single rule. Cancellation outranks
// everything, because a killed process exits non-zero and its stderr is
// whatever it was midway through saying. Infrastructure outranks work, because
// a rate-limited run produced no work to judge.
//
// A refused tool is deliberately not a class. The CLI exits 0 and reports
// subtype "success" for a run it refused every write on, so there is nothing
// here to classify — and a refusal is not a failure by itself, since a run can
// be refused one tool, take another route and finish. The refused tool names
// ride on Result.Denials instead, and the engine reads them where they become
// evidence: a round that was refused a tool and then changed nothing.
func classify(o outcome) runner.Class {
	switch {
	case o.startErr != nil:
		// A missing or unrunnable CLI is an environment problem, not a verdict
		// on the issue.
		return runner.ClassInfraFailed
	case o.ctxErr != nil, o.timedOut:
		// Nothing here is a verdict: the run is resumable and costs neither a
		// round nor an attempt.
		return runner.ClassInterrupted
	}

	failed := o.exitCode != 0 || o.resultErr || !o.sawResult
	if !failed {
		return runner.ClassOK
	}
	// Ahead of the prose, because it does not depend on how the CLI worded this
	// week's limit. A result line saying subtype "success" while carrying
	// api_error_status 429 is exactly the shape a session limit arrives in, and
	// reading the subtype instead sends five workers into the same wall.
	if apiOutage(o.apiStatus, o.terminalReason) {
		return runner.ClassInfraFailed
	}
	if infraSignal(o.stderr, o.failText) {
		return runner.ClassInfraFailed
	}
	if !o.sawResult {
		// The CLI exited without ever reaching a verdict: it crashed, or it
		// died on something it never got to report through the stream.
		return runner.ClassInfraFailed
	}
	// The run happened and reported failure — max turns, an execution error.
	// That is work to feed back, not an outage to retry.
	return runner.ClassWorkFailed
}
