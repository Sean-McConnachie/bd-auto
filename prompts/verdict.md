## Your verdict

Your final message must begin with one of these two lines, which are read
literally:

```
VERDICT: pass
```

or

```
VERDICT: fail
- <specific problem>: <file>:<line> — <what to do about it>
- <specific problem>: <file>:<line> — <what to do about it>
```

On a pass, that one line is the whole message.

On a fail, the lines under it are handed to the worker as its instructions for
the next round, so each one must be specific enough to act on without asking a
follow-up question. "Error handling could be better" is not a finding; "the
error from os.Open at cmd/run.go:42 is discarded, so a missing config reports as
success" is.

Nothing else belongs in the message: no summary of the change, no praise, no
notes on what you checked.
