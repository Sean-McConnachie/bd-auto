package similar_test

// corpusFixture is this repository's whole backlog at the time the threshold
// was set: 93 issues, which is the corpus a real barrier indexes. Rarity
// weighting is only meaningful against a corpus that size, so the test uses
// the real one rather than a handful of samples.
//
// It is pasted rather than read from .beads/issues.jsonl so the test keeps
// its meaning after the backlog moves on.
var corpusFixture = map[string]string{
	"joa": `Unstaging the beads export once per barrier is undone by the next bd read

Live drain 2, wave 4: the only issue in the wave finished, gated and reviewed, and the barrier then parked it with 'git would not merge bd-auto/kv-555.4 and left no conflicted paths: Your local changes to the following files would be overwritten by merge: .beads/issues.jsonl'. beads-auto-imp-dql.9 was supposed to have fixed exactly this, and the fix is in the binary that ran.

Root cause: unstageBeadsExport() is called once, at the top of Integrate, and every bd command run after it puts the export straight back in the index. Reproduced in the lab: 'git status --porcelain -- .beads/issues.jsonl' says ' M', 'bd show <id>' is run, and it says 'M '. A read command re-exports and git-adds. candidates() calls e.BD.Show for every candidate, between the unstage and the merge, so by the time git merge runs the index is dirty again and ort refuses.

The unstage has to happen immediately before the merge itself, not once per barrier. Verified by hand in the failing lab repo: reset HEAD -- .beads, then the identical merge command succeeds.`,
	"dql.9": `a worker's commit stages the beads export into the main checkout, and the next barrier cannot merge anything

Found by the live kv-ctf drain, which finished three issues — all gated, all reviewed, all committed — and parked two of them at the wave 2 barrier with:

    git merge --no-ff --no-edit bd-auto/kv-ctf.2: exit status 2:
    error: Your local changes to the following files would be overwritten by merge:
      .beads/issues.jsonl

The chain, reproduced by hand:

1. A worker commits in its own worktree. Its git is not suppressed — deliberately, because beads' pre-commit is what keeps the export in step — so beads' pre-commit hook runs.
2. bd exports .beads/issues.jsonl and stages it. The export lives in the main checkout, and so does the index it is staged into: a commit inside .beads/auto/wt/<issue> leaves  staged in the main checkout, not in the worktree that made the commit.
3. The barrier merges into the epic branch in that main checkout. git refuses to merge while a path in the index differs from HEAD, even when the branch being merged does not touch that path — ort writes a fresh index and will not overwrite a staged change.

Every branch after the first worker commit therefore fails to merge, is recorded as parked, and the handoff is refused over work that was already finished. The run reported  above a table whose three rows all said done.

Verified both halves separately: with the export merely modified in the working tree the same merge succeeds; staged, it fails. The workaround in this repo's own history is a human running  between waves — see the two commits named 'bd: export issue state ahead of <issue> merge'.`,
	"eqc.1": `Spike: verify concurrent bd writes from N worktrees

Before anything else, prove that 5 concurrent processes can safely hit the one embedded Dolt DB from 5 different git worktrees. Everything else in this epic assumes it.

Test: create 5 worktrees, run bd ready --claim simultaneously from all of them against a pool of 5 open issues, and confirm each process claims a DIFFERENT issue with no errors, no lock timeouts and no corruption. Repeat for concurrent bd close, bd note and bd create.

If direct embedded mode does not hold up, evaluate the alternatives bd already ships: the auto-started Dolt server (see the dolt-server.* entries in .beads/.gitignore), bd --global shared-server mode, and --dolt-auto-commit=batch. Record the verdict in the design field of the epic.`,
	"ej3": `Triage gate: a discovery is staged for a human, not filed as an issue by the barrier

Measured over this project's whole history, agent-discovered work peaked at 2.27 issues created per issue closed (34 created / 15 closed across the wz9 drains, 19 of the 34 carrying a discovered-from edge). The discovery edges show the shape: nine different parent issues each produced exactly two children (wz9.2, wz9.6, wz9.7, wz9.8, wz9.11, j5a.6, o4q, 908, tv9). Real discovery is lumpy; a constant two per issue is form-filling.

beads-auto-imp-fqu moved filing from the worker to the barrier and beads-auto-imp-qhs put a bar in the prompt, and both helped. What neither changed is that the barrier still runs bd create, so the issue count grows whether or not anything was learned. DiscoveredDefer hides the result from bd ready, which protects the next run, but the backlog a human reads is the thing that actually grows.

Add a third mode to discovered_work: triage. Under it the barrier files nothing. Discoveries accumulate in a durable staging file that outlives the run, and a new command bd-auto triage is what turns one into an issue, folds it into an existing issue as a note, or discards it. Nothing becomes a bd issue until a human says so.`,
	"1sx": `The reviewer role can write issue state, and did: a review ran bd close on the issue under review

From .beads/auto/logs/beads-auto-imp-46o-a1-r0-review.jsonl, the reviewer says: "I mistakenly ran bd close beads-auto-imp-46o and bd update ... --notes=... during this review... reviewers must not touch issue state." beads-auto-imp-46o close_reason is now the literal string "review only, not closing".

A reviewer is meant to return a verdict. It should not be able to close, reopen, comment on or re-note the issue it is judging: a review that mutates the record can close work that never landed, and the engine trusts issue state when it decides what to merge and when an epic settles.

The reviewer already runs under permissions: scoped. The allowlist evidently permits bd writes. Restrict the reviewer to read-only bd (show, list, ready) and deny the mutating verbs, so this is refused by the harness rather than by instructions in a prompt.`,
	"1ud": `scope.Blocked never checks whether the scoped issue is itself deferred

internal/drain/scope.go:326-345 walks an issue dependencies to decide if it is blocked, but never asks whether the issue itself carries a deferral. beads-auto-imp-j5a.2 fixed Candidates and candidateSet, so a deferred issue no longer enters a scope preview — but an issue named explicitly in --issues bypasses that path. parkOutOfScope therefore cannot catch it: the issue sits unready for the whole run and falls to the end-of-run sweep.

This is what produced drain 5 seventeen stranded issues. It is the real bug behind the note that reads "never became ready, and the run drained without bd ever offering it".`,
	"3k2": `Test run: stress flob (refs/dolt/flob) across drain worktrees and prove no records are lost

The drain test run and scripts/smoke.sh exercise the beads Dolt store only. flob (~/dev/full-observability) is a second Dolt-backed tool that travels on the git remote under refs/dolt/flob, a ref it took deliberately so it can sit beside the beads ref refs/dolt/data (see src/flob/store/dolt.py DATA_REF and src/flob/cli/sync_cmd.py).

A bd-auto drain runs N worker worktrees off one checkout and merges them at a barrier, so every hazard beads has already hit applies to flob as well, over data that is far more expensive to lose: flob records experiment runs, and a dropped record costs the training time that produced it.

Extend the test run to cover a repo where flob is initialised and being written while a drain is in flight. Workers record flob runs concurrently, the integrator merges their branches, and the test then proves every record written is still readable and that refs/dolt/flob advanced without losing a Dolt commit.

Hazards to cover explicitly:
- a barrier merge that resolves refs/dolt/flob by taking one side, silently dropping the other side records. The beads analogue is beads-auto-imp-xdv, where every branch in a wave carries a full export of one shared database.
- worktree teardown or gitguard removing or rewinding a flob store a worker has just written.
- two workers running flob push against the same ref inside one wave.
- a gate failure, a peel-back or a retry that resets a branch and takes flob commits with it.

SAFETY, and this is the point of the issue: the test must build its own throwaway repo and its own flob store, the way scripts/resume-vs-fresh.sh already builds a fixture repo. It must never run against ~/dev/full-observability or any other real flob store. Records there are not reproducible without paying for the training that produced them again.`,
	"1p8": `Concurrent 'git worktree add' races, and a wave of five can lose an issue to it before any model runs

internal/worktree/worktree.go Ensure/Create runs 'git worktree add' with no serialisation, and drain_wave.go dispatches one per issue concurrently — the comment at worktree.go:100 already notes that 'a wave runs several Ensure calls at once'. Concurrent adds race inside .git/worktrees: git fails with 'fatal: failed to read .git/worktrees/<other-issue>/commondir: Success' while another add is mid-write.

Reproduced on a585bf5 with no changes applied: 'go test ./internal/drain/ -count=12' failed TestDrainRunsADagAcrossWavesAndParksWhatFailed twice with that error, and TestAnOutageStopsTheWaveWithoutParkingAnything once on a separate run. It only shows under load — the tests pass 120/120 when run alone — which is why it reads as an unrelated flake rather than what it is.

It is not only a test flake. The engine reports the failure as 'worktree: create ...' from Engine.Issue, which is a returned error rather than an outcome, so the whole Drain call fails and takes its siblings with it. concurrency defaults to 5, so a real run makes five of these calls at once against the same repository.

A fix has to serialise worktree creation across a wave — a mutex in internal/worktree around the add, or a queue in the dispatcher — and cover it with a test that creates several worktrees concurrently and asserts they all land.

Found by the bd-auto worker on beads-auto-imp-j5a.6, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"j5a.3": `A worker that parks itself is recorded done and merged, because Terminal() treats blocked as finished

prompts/worker.md step 7 tells a worker that cannot finish to run:

    bd update <issue-id> --status=blocked --append-notes="bd-auto attempt: <what blocked you>"

The engine then reads that as success.

internal/drain/issue.go:261 checks 'if !cur.Terminal()', and bd.Issue.Terminal() is Closed() || Blocked() (internal/bd/bd.go:64). A blocked issue passes. The attempt goes on to the git guard and the review stages and, if they pass, returns OutcomeDone (issue.go:290) — so recordDone marks it done, its branch is merged at the barrier, and the run reports it as completed work.

It gets worse at the barrier. reconcile() (internal/drain/reconcile.go:82) walks st.Done, sees bd has the issue blocked rather than closed, and re-closes it with a reason blaming beads' post-checkout hook for reverting a write that was never reverted — the worker set that status deliberately. The one signal the worker had to say 'I could not do this' is overwritten with a sentence saying the tracker lost an update.

This is how a worker's 'I am blocked by something else' turns into a green run. It is the mechanism behind the report that motivated this epic: a worker declared its issue out of scope until another issue finished, and the run did not treat that as a park.

Terminal() is right for the places that ask 'may a worker stop here' — parkOutOfScope and parkStranded both use it to mean 'already settled'. It is wrong as the completion test for an attempt.`,
	"j5a.2": `Deferred issues are counted as workable: they enter scope previews, block epic closure and inflate run status

bd's list and stats do not subtract deferred work, and bd-auto believes them.

Measured in this repo today:

    bd list --json --limit 0   -> 19 open, 16 of them with defer_until in 2029
    bd stats                   -> Open: 19, Ready to Work: 19
    bd ready --json            -> 2

So 'Ready to Work: 19' is wrong by 17. bd-auto manufactures these itself: discovered work is filed deferred at +1000d (DiscoveredDefer, internal/drain/discover.go:209), which is the right call — it keeps discovered work out of the run a human approved — but it means every run leaves behind issues that later runs then mistake for work.

Where it lands:

- internal/bd/bd.go:158 Children() runs 'bd list --parent --all --limit 0 --flat' and gets the deferred children back. scope.Candidates (internal/scope/scope.go:95) skips only closed and blocked ones, so a deferred child is offered to the human as something to scope a run to. Pick it and bd ready never offers it, the planner never dispatches it, and parkStranded parks it at the end of the run with 'never became ready, and the run drained without bd ever offering it' (internal/drain/drain_wave.go:702) — true, and no help at all.
- EpicComplete (internal/drain/integrate.go:755) counts any unclosed child as open. A deferred child therefore lands in v.Open and, being outside the scope, in v.OutOfScope, so the epic can never close and the reason a human reads names issues nobody can work.
- EpicStats (internal/bd/bd.go:319) counts them Open, so 'bd-auto run status' (internal/cmds/run.go:205) reports work that is hidden from bd ready by construction.
- scope.Blocked (internal/scope/scope.go:273) parks an in-scope issue whose blocker is out of scope. It does not consider a blocker that is IN scope but deferred, which can never become ready either.

bd.Issue (internal/bd/bd.go:38) has no defer_until field at all, so none of this code can currently tell the difference.`,
	"j5a": `bd-auto: a transcript view, deferred-issue blindness, and wave slots that refill

Three things a live drain does badly, plus two defects found while confirming them.

1. The wave table shows one line per issue and nothing else. There is no way to see what a worker actually did — the transcripts are on disk at .beads/auto/logs/ and invisible from the view that is watching them stream past.

2. bd's own list and stats count deferred work as open and ready. bd-auto manufactures deferred issues itself (discover.go files discovered work at +1000d), so every run poisons its own later runs: deferred children become scope candidates, block epic closure, and inflate run status.

3. A wave is planned at exactly the concurrency cap and never grows. An issue that parks in the first minute leaves its worker slot idle until the barrier, while in-scope issues sit in the table saying 'waiting'.

Found while confirming (2) and (3): a worker that does what prompts/worker.md step 7 tells it to do when blocked is recorded as done and merged, and nothing in the run or the prompt tells a worker that a sibling running beside it is not a blocker.`,
	"wne": `The barrier is invisible: a model resolving a conflict shows as an idle table

Drain 3 put two workers on the same function on purpose. Both finished, the barrier could not merge the second branch, and bd-auto spawned an integrator to resolve it — a model call that ran for minutes and cost real money.

For all of that the live view said:

    0 running · 2 done · 0 parked · 0 killed · run total $2.5144
    ↑/↓ select · k kill the selected worker · q stop the run

Nothing about a barrier, nothing about a conflict, nothing about a model in flight. A watcher cannot tell a run that is integrating from a run that has hung, which is the one distinction the display exists to make — the same argument the row's own activity column is built on.

The engine knows: it merges in order, it logs '<issue>: <branch> conflicts in <paths>', and the integrator streams events like any other process. The bus has no event for the barrier opening, so the view cannot say so. It needs one: a wave-integrating event when merging starts, and the integrator's activity routed to the row of the issue whose branch it is resolving.`,
	"crh": `A row parked by the barrier keeps saying done, and the counters keep counting it

Drain 2's last leg ended with the table saying '1 done · 0 parked' directly above the run's own verdict, 'run done after 1 wave(s): 3 done, 1 parked'. The issue in question, kv-555.4, had finished its worker and then been parked at integration because the merge refused: its work did not land.

The view learns this. EventWaveEnd carries the whole IntegrateReport, and the model uses it for one thing only — a status line saying how many merged and how many parked. The rows are never touched, so an issue the barrier parked keeps the state its worker left it in, and every counter derived from the rows keeps counting it as done.

A row should follow the barrier's verdict: an issue in Integration.Parked() becomes parked, with the merge's reason as its activity, so the table and the run agree.`,
	"dql.8": `every wave is reported one lower than it is, so the first wave has no number at all

The drain loop loads run state, plans a wave, and then emits EventWaveStart, EventIssueStart and EventIssueEnd with st.Wave from the copy it loaded. But the thing that advances the wave counter is wave.Record, which writes it to disk and returns the updated state — and nextWave throws that return value away. So the caller's st.Wave is still the number of the wave before this one.

The first wave is therefore emitted as wave 0, which the table renders as a dash in the WAVE column and drops from the heading entirely, and the second as wave 1. The barrier does not share the fault: Integrate reloads run state, so EventWaveEnd carries the real number — which means one screen says 'wave 0' in every row while the status line under it says 'wave 1 integrated'.

Seen in the live kv-ctf drain: three issues, two waves, and the WAVE column read '-' throughout.`,
	"dql.4": `Live drain 2: the cli epic under wave autonomy, with a kill, a park, an unpark and a question

Exercise the failure and human-in-the-loop paths against real models: pause at a wave barrier and resume, answer a worker question from the table, kill a worker from the table, confirm the handoff refuses over a parked issue, unpark it and finish the run.`,
	"dql.2": `Deterministic TUI screenshot harness over a real PTY

Drive the real tui.UI in a tmux pane with synthetic drain events and real keystrokes, covering every state the table and the question box can be in, and capture each one. Env-gated so go test ./... is unaffected.`,
	"dql.3": `Live drain 1: the store epic, happy path, epic branch and pull request

Real workers, two waves, integration at each barrier, the epic closing in beads, the epic branch pushed to the local remote and a pull request opened through the shim.`,
	"dql.1": `Build the throwaway test lab: repo, local bare remote, gh shim, beads DAG

A fresh git repo outside this one, a Go module for the program under test, a bare repo on disk as origin so a push never leaves the machine, a recording gh shim on PATH so the pull-request path runs without GitHub, and two beads epics whose dependency graph produces at least three waves.`,
	"dql": `Epic: end-to-end live validation of bd-auto in a throwaway repo

Synthesize a small-scale but complete bd-auto exercise in a fresh repo: a real Go program built by real workers across two epics, several waves, a killed worker, a parked-then-unparked issue, a worker question answered from the TUI, and a handoff that pushes to a local bare remote and opens a pull request through a recording gh shim. Nothing touches GitHub. Every TUI state is screenshotted, and token usage is recorded from the run's own state.`,
	"omc": `flob leaks a detached dolt sql-server per repro run and per untorn-down test project

244 orphaned 'dolt sql-server' processes accumulated on this machine (~6.3 GB RSS, oldest 3 days), all reparented to systemd --user. Source is flob (~/dev/full-observability), not beads.

DoltStore.start() (src/flob/store/dolt.py:230) spawns the server with start_new_session=True, deliberately detached so it outlives the CLI. close() explicitly keeps the server running. There is no atexit hook, no finalizer, and no idle timeout, so only an explicit DoltStore.stop() ever reaps a server. Three paths create stores and never call it:

1. flob repro (125 leaks). repro.py:153 mkdtemps a /tmp/flob-repro-* worktree; _prepare_scratch (repro.py:284) gives it a fresh .flob/dolt; the child re-run starts a detached server there. The finally block calls _remove_worktree (repro.py:235) which does git worktree remove + rmtree + prune, but never stops the server. One leak per invocation, each left pinned to a deleted directory.

2. Test suite (111 leaks). conftest.py:96 stops the server, but only on the 'project' fixture. Six tests bypass it and leak once per pytest run: test_cli.py:52,72,97,124 (bare 'repo' fixture then flob init), test_sdk.py:452 (project built under tmp_path), and test_cli.py:440 (uses 'project', but also creates a second project at tmp_path/other which is never stopped). test_sync.py already patches this per-call-site in three places, which is the same bug being fixed one site at a time.

3. Git worktrees (6 leaks). theological-LLMs/.claude/worktrees/agent-*/.flob/dolt servers outlived their worktrees, which were removed after merge.

The 242 leaked processes have been killed; the 2 live project stores were left running.`,
	"nwu": `bd-auto hook: a missing subcommand wedges a whole Claude Code session, tools and turn-end alike

A Claude Code session in this repo registers two hooks that point at a
subcommand bd-auto does not have:

    "${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto hook pre-tool-use
    "${CLAUDE_PLUGIN_ROOT}"/bin/bd-auto hook stop

bd-auto has no 'hook' command, so it prints its usage and exits non-zero.
Claude Code reads that exit as "block", and the result is a session that
cannot do anything:

- PreToolUse: EVERY Bash call is refused before it runs. Not a class of
  commands — every one, 'true' included. git, go, bd: all unreachable.
- Stop: every attempt to end a turn is refused, so the model is re-invoked
  immediately, forever.

Observed for real, not hypothesised. The session that hit this burned
roughly forty turns alternating "still blocked" and "idle" before Claude
Code's own backstop fired: "A hook blocked the turn from ending 9
consecutive times - overriding and ending turn." Until that cap, the loop
was unbounded and self-sustaining: the agent could neither act nor stop,
and could not fix the hook, because fixing it needs the shell the hook is
blocking. The only escape found was the Monitor tool, which runs commands
in the same shell but is not covered by the hook's matcher. That is a hole
in the sandbox, not a supported route, and it should not be the thing
standing between a wedged session and its work.

Where the config lives is itself part of the bug: it is nowhere. Searching
~/.claude, ~/dev and ~/.config for "hook pre-tool-use" and for
CLAUDE_PLUGIN_ROOT turns up only conversation transcripts - no
hooks.json, no settings entry, and .claude-plugin/plugin.json declares no
hooks at all. The hooks were loaded into a running session from config
that has since been removed, and they persist in that session's memory
until it restarts. So the operator's fix is "restart Claude Code", which is
not discoverable from the error, and there is no file to edit.

This is bd-auto's problem to prevent, whatever put the config there. We
ship the plugin, and a plugin whose hook can wedge a session has to fail
open. A hook is on the path of every single tool call; it is the one place
in the system where exiting non-zero on an unrecognised input costs the
user their whole session.`,
	"fqu": `Discoveries are filed by bd-auto at the barrier, not by the worker

A worker filing its own discovered work with 'bd create' has two problems. It has no view of what any other worker filed, so the same finding lands twice — beads-auto-imp-pzi and beads-auto-imp-6up are the same work filed by two workers three waves apart. And the create is a bd write from inside a worktree, which is exactly the class of write beads-auto-imp-g7t says does not survive a checkout-shaped git operation.

Move the filing to bd-auto. The worker writes what it found to .beads/auto/discovered.json in its worktree (gitignored, so 'git add -A' cannot commit it); bd-auto harvests that file at the end of every attempt into run state, and files the issues itself at the barrier, deduplicated by normalised title against both what this run already filed and every issue already in bd.`,
	"buu": `ask_user MCP tool: let a headless worker ask the human a question without ending its session

A headless worker that hits a genuine ambiguity has no way to ask. Today its only options are to guess, or to fail the attempt and let the failure text reach a human on the next round — both expensive and both lossy. Meanwhile the interactive orchestrator path CAN ask, and that turned out to be one of the more valuable things it did.

Give the headless engine the same ability, as a tool rather than as a session exit.

Why a tool specifically: exiting the 'claude -p' process to ask means the next attempt is a new session that must be re-sent the whole context, which is exactly the cost this project exists to avoid. A tool call blocks inside the live session, so the answer costs one tool round-trip instead of a session restart.

Shape:
- bd-auto exposes an MCP server (stdio) offering an ask_user tool, passed to the backend at spawn — for the claude adapter that is --mcp-config plus the tool on the allowlist. The Runner seam already exists, so this must not be claude-specific in the engine; a backend that cannot offer tools should degrade, not break.
- The tool takes a question and a small list of options, matching the shape the Claude Code AskUserQuestion tool uses, since that shape is already proven and models already know it.
- A call blocks the calling worker only. Other workers keep running.
- bd-auto raises the question on the existing drain.Bus; the TUI from beads-auto-imp-wz9.10 renders it as a popup over the wave view and sends the answer back as the tool result.
- Several workers may ask at once. The TUI needs a queue and must make clear which issue is asking; the wave view already has a per-issue row to hang that off.

The hard part is the unattended case, and it is what will make or break this:
- --quiet, --plain, --json and any non-TTY run have nobody to answer. A question must never hang a drain forever.
- Needs an explicit policy with a safe default. Suggested default: answer unavailable, the tool returns promptly telling the model to proceed on its best judgement and record the assumption in the issue, so an unattended drain degrades to today's behaviour rather than stalling.
- A timeout is needed even WITH a TTY, since the human may walk away.
- Questions and answers must be recorded in run state, so a resumed run does not re-ask what was already answered, and so a human reading the run afterwards can see what was decided and why.`,
	"908": `Epic branch + PR handoff: merge a run into a temp branch and open a PR for human review

Today a drain merges each completed issue branch straight into the base branch (main), so a run publishes itself with no human gate. Change the default so a run is staged on a temporary epic branch and handed to a human as a PR.

Behaviour:
- By default, a run over an epic (or any issue set) merges each issue branch, in dependency order, into a temporary epic branch rather than into the base branch.
- Conflicts during those merges use the model, exactly as the existing integrator does — the conflict-only role, resolve-and-stage.
- Only once every issue has merged cleanly and the gate passes on the fully merged epic branch is a PR opened against the base branch.
- The PR is the handoff point: a human reviews and merges. bd-auto never merges to base itself in this mode.
- If any issue is parked, or the gate is red on the epic branch, no PR is opened; the epic branch is left in place for inspection.

Configuration:
- On by default.
- The PR step can be turned off, in which case the epic branch is still produced and left for the human to do what they want with. Decide whether the branch-vs-base-merge behaviour is a separate switch from the PR switch.`,
	"2qx": `Swarm: bd-auto: headless engine with a swappable agent backend

Swarm molecule orchestrating epic beads-auto-imp-wz9.

Epic: beads-auto-imp-wz9
Coordinator: `,
	"wz9.9": `Scope selection and bd-auto drain

The human bound on a run, and the wave engine that respects it.

drain does not drain an epic, it drains a set of issues a human named. There is
no automatic limit on spend anywhere in this design, so the bound is applied up
front, at the only moment the user can see what they are agreeing to.

- On a TTY: compute the candidate set (the epic's open, unparked children), show
  it with the wave decomposition, the gate commands and the model chosen per
  role, and require an explicit multi-select and confirmation. Nothing spawns
  before the confirm. It is a preview of the shape of the spend, not a yes/no.
- Off a TTY (skill launcher, CI, --plain): scope must be explicit, via
  --issues a,b,c or --epic X --all. A bare --epic X with no TTY and no explicit
  selection prints the candidate set and EXITS NON-ZERO without dispatching. The
  background path is the one with nobody watching, so it is the one that must
  name its work.
- Selected IDs go to run state as Scope before anything runs. wave.Plan
  intersects bd ready with Scope; an issue outside it is never dispatched,
  whatever bd says about its readiness.
- Discovered work is out of scope by construction: workers file it deferred, and
  a deferred issue was not in the approved list.
- An in-scope issue whose dependency is out of scope and unmet is parked
  immediately with that reason, rather than sitting unready until the run ends
  unable to explain itself.
- The run ends when every scoped issue is done or parked. The epic may still be
  open, and epicComplete refuses to close it unless every child completed.

Then the engine around it: wave loop, errgroup bounded by concurrency, event
bus, plain and --json renderers, and interrupt recovery (resume the persisted
session when the worktree survived, else fresh; attempt counter unchanged
either way).

autonomy shrinks to auto | wave. wave still pauses at each barrier and waits for
bd-auto run resume, which is useful when the scope is large. issue is deleted:
pausing after each issue is redundant when the human already chose the issues.

Interrupt recovery has one known hazard, and it is specific. A claude -p killed
mid-turn can leave a transcript ending in a tool_use block with no matching
tool_result; resuming that sends an invalid message sequence and errors
immediately. With the Class taxonomy that is an infra-failed on the first
resumed turn, which falls back to a fresh dispatch automatically. That is what
makes the least-tested path in the system self-healing rather than a coin flip.`,
	"wz9.7": `Measure resume vs fresh, then set the defaults

The recovery design rests on a number nobody has measured yet, so measure it
before choosing the default rather than after.

The claim is that resuming is cheaper than a fresh worker because a fresh worker
re-reads the issue, re-explores the code and re-derives its plan first. The
counterargument, stated honestly in the plan: a resumed session re-sends its
whole transcript every turn, so its per-turn input is strictly larger. The
cache-warmth that would offset this depends on a five-minute TTL, and between
rounds we run the full gate (go build && go vet && go test) and possibly a
reviewer, which can comfortably exceed it. The transcript also grows every round
until it autocompacts, at which point the warm prefix is gone and a compaction
has been paid for.

Drain the same small epic twice with bd-auto issue run:
  max_rounds: 1, retry: 3   (fresh-heavy)
  max_rounds: 4, retry: 1   (resume-heavy)

Compare accumulated total_cost_usd, NOT token counts: cache reads bill at a
fraction of input price and a raw token count flatters whichever side reads more
cache.`,
	"wz9.6": `Per-issue pipeline and bd-auto issue run --issue X

One issue end to end, standalone and debuggable before any wave logic exists.
The unified recovery loop with one feedback channel:

  mark := worktree.Snapshot(wt)                    // branch tip + dirty set
  res  := runner.Run(...)
  infra-failed  -> backoff, re-run the same round  // no round, no attempt spent
  interrupted   -> return; attempt counter untouched
  !Changed      -> hard fail the attempt           // NOT another round
  !Terminal     -> feedback, next round
  guard fails   -> feedback, next round
  gate fails    -> feedback, next round
  review fails  -> feedback, next round

The progress check must come first and must be a hard failure. Every check below
it is satisfiable by stale state: after round 1 the issue is already closed, so
Terminal() passes on round 2 even if round 2 did nothing, and gitguard.Verify
passes on an unchanged branch. A no-op round would sail through to the reviewer,
which re-reads an identical diff, fails identically, and spends every remaining
round in an empty loop at full price.

Sessions persist to run state (Attempt.AgentID at state.go:38 becomes
WorkerSession and ReviewSession, written before the runner is invoked rather
than by a hook afterwards). Review notes go to .beads/auto/review/<id>.md.

The reviewer defaults to a FRESH session. Reviewer resume is available but off:
a resumed reviewer carries its own previous VERDICT: fail and is anchored to
checking whether its findings were addressed rather than re-judging the diff,
and it is the cheap half anyway since the diff dominates its input and must be
re-read after changes.`,
	"wz9.5": `internal/worktree and internal/gitguard: own the worktree and close the git holes

Worktree creation is currently Claude Code's job via 'isolation: worktree' in the
agent frontmatter. Verified: 'git worktree add' appears nowhere in the Go code,
only listWorktrees (merge.go:201) and 'git worktree remove' (worker.go:174).
Deleting the plugin deletes worktree creation, so Go has to own the lifecycle.

internal/worktree:
- create at .beads/auto/wt/<issue> from the base SHA recorded at dispatch
- reuse on a later round and on a resumed run. The path must be stable, because
  'claude --resume' resolves a session against the project derived from cwd; a
  moved worktree is a lost session
- 'git worktree prune' at drain start, plus adopt-or-recreate for the
  "worktree exists, branch does not" case
- removal only between attempts and after a successful merge

internal/gitguard replaces the PreToolUse deny at hook.go:280, whose
agent_id == "" guard (hook.go:348) silently stops firing headless.

Structural, at worktree creation. Two traps, both confirmed against this repo:
- --worktree and extensions.worktreeConfig are BOTH required; a plain git config
  inside a worktree writes the shared config and breaks the main checkout.
- core.hooksPath is ALREADY set here, to .beads/hooks, holding beads' pre-commit,
  pre-push, post-checkout, post-merge and prepare-commit-msg. Pointing it at a
  rejector dir disables every one of them inside worker worktrees, including the
  pre-commit that keeps issues.jsonl in sync. So the rejector dir is generated
  per run and CHAINS: reject if it is one bd-auto blocks, else exec the
  same-named hook under the previous hooksPath with "$@" (falling back to
  .git/hooks). Its own prepare-commit-msg appends a Bd-Auto: <issue>/<attempt>
  trailer before chaining.

What the structural half cannot catch: pre-merge-commit does not fire on a
fast-forward merge, and git rebase has no hook at all. pre-push does fire for
every push regardless of remote, so the push half is genuinely closed once
chaining works.

Post-hoc, against the base SHA recorded at dispatch:
- git merge-base --is-ancestor <base> <branch>
- git rev-list --min-parents=2 --count <base>..<branch> is 0
- EVERY commit in <base>..<branch> carries this run's Bd-Auto: trailer
- base branch SHA unchanged
- refs/remotes/origin/* unchanged

The trailer check is the one that closes the holes. After 'git rebase
origin/main' the base is still an ancestor, there are still no merge commits,
and the local and remote refs are still unchanged, but base..branch now contains
origin's commits and those have no trailer. Same for a fast-forward merge.`,
	"wz9.2": `runner/claude and runner/fake adapters

The only shipping backend, plus the scripted adapter that makes the whole engine
testable with zero model calls.

claude -p <task> --append-system-prompt <role> --model <m> --permission-mode auto
  --session-id <uuid> | --resume <uuid>
  --output-format stream-json --verbose --include-partial-messages
  --allowed-tools ...   (reviewer only)

All verified present in CLI 2.1.233. --include-partial-messages is what makes
the TUI activity line text-granular rather than tool-call-granular.

Two details that are easy to get wrong and expensive to debug:

- Cancellation must kill the process group. exec.CommandContext kills the direct
  child only, so a worker 40s into 'go test ./...' leaves the test running and
  holding its worktree. Set SysProcAttr{Setpgid: true} and signal the group
  (SIGTERM, then SIGKILL after a grace period).
- Usage is recorded as cost, not summed tokens. Across resume rounds the same
  prefix is billed repeatedly as cache reads at a fraction of input price, so
  summing input_tokens overstates a resumed attempt. Record total_cost_usd plus
  input/output/cache-read/cache-creation separately.

fake replays scripted results including Class, and records every Request it
receives so downstream tests can assert exactly what was asked for.`,
	"wz9.1": `internal/runner: Runner interface, Class taxonomy and per-role config

The seam everything else plugs into. Defines Runner, Request, Result,
Capabilities, Permissions, and the Class taxonomy the engine branches on before
anything else:

  ok | work-failed | infra-failed | interrupted

Class exists because "the process exited non-zero" is not one thing. Without it,
five parallel workers meeting one rate limit each burn three rounds and a fresh
attempt against a 429 and then park five good issues with a nonsense reason.
infra-failed must consume neither a round nor an attempt.

Also lands the runners: config block with per-role resolution over default, and
the two validation rules called out in the plan: a pipeline stage's agent: field
now names a key under runners: (a silent breaking change), and per-stage
max_rounds wins over the run-level setting.

Plan: plans/headless.md, sections "The Runner seam" and "Config".`,
	"wz9": `bd-auto: headless engine with a swappable agent backend

Invert bd-auto: Go owns control flow and spawns models as subprocesses, instead
of a live orchestrator context dispatching subagents through the Agent tool.

Today coordination is O(issues) in one append-only transcript that is re-sent
every turn, which is the context blow-up this project exists to fix. Model
invocation goes behind a Runner interface so the backend can be swapped without
touching the engine.

What bounds a run is a human-selected set of issues, chosen before any model is
spawned. There are no budgets, no per-request timeouts and no circuit breakers.

Full plan: plans/headless.md`,
	"tv9": `Plan: persistent graphify-backed codebase index initialised at each wave start

We need a human plan/decision before implementing.

Idea: at the start of every bd-auto wave, initialise a persistent tool/service backed by graphify (the knowledge-graph skill) over the current codebase. Worker, reviewer and integrator agents then query that tool instead of grepping and reading files, so they can understand the codebase at a much lower token count.

Open questions for the human:
- Where does the graph live (graphify-out/ in the main checkout? shared read-only mount into each worktree?) and who owns its lifecycle?
- When is it built/refreshed — once per wave at dispatch, or incrementally after the integrator merges each wave?
- How do agents query it: a slash command / skill invocation, an MCP server, or a plain CLI the agents shell out to?
- Staleness policy: worktrees diverge from the base branch during a wave, so how do we signal that graph answers may lag the worker's own edits?
- Cost/benefit: graph build time and tokens vs. the per-agent context savings. Is it worth it below some issue count?
- Fallback: what agents do when the graph is missing, stale or the build fails.

Deliverable: a written plan (design doc or a set of child issues) that a bd-auto wave can then implement.`,
	"eqc.12": `Plugin packaging and required repo settings

Package as a proper Claude Code plugin in this repo: .claude-plugin/plugin.json plus skills/, agents/, hooks/hooks.json, bin/. Load during development with claude --plugin-dir . and validate with claude plugin validate.

Repo settings this design REQUIRES, documented in the README and applied here:
- worktree.baseRef=head in settings, otherwise subagent worktrees branch from the default branch and a wave will not see the previous wave merge.
- .claude/worktrees/ added to .gitignore.
- .worktreeinclude if any gitignored file is needed inside worker worktrees. Note .beads/embeddeddolt must NOT be copied; bd already resolves the main repo database from inside a worktree.
- The session must run in an accepting permission mode. permissionMode in frontmatter is ignored for PLUGIN subagents, so five workers in a prompting mode would produce a prompt storm.
- Use ${CLAUDE_PLUGIN_ROOT} for every hook command path.`,
	"eqc.11": `Failure policy: retry once fresh, then park

Uniform handling for a worker that errors, a gate that will not pass, or a review that fails twice.

First failure: discard the attempt (remove its worktree and branch), retry the issue with a FRESH worker seeded with the failure notes appended to the issue. Second failure: bd update --status=blocked --append-notes with the accumulated evidence, flag with bd human, and continue the run. A single bad issue must never stall the drain.

Retry count is configurable. Every attempt appends to the issue notes so the history is in beads rather than in a context window that is about to be compacted.`,
	"eqc.10": `Skill and slash command: the orchestrator protocol

skills/bd-auto/SKILL.md defines what the main session does: validate the DAG, start the run, compute a wave, dispatch all workers for that wave IN A SINGLE MESSAGE so they run in parallel, collect capped reports, run the review stage (resuming the same worker with feedback on rejection, max 2 rounds), hit the wave barrier, invoke the integrator, then loop.

Keep it SHORT. It is re-injected by the SessionStart/PostCompact hooks, and a long skill defeats the purpose. Anything that can be enforced by the binary or a hook does not belong in prose here.

Entry point /bd-auto <epic-id> with flags for concurrency, autonomy mode and dry run. Dry run prints the wave plan and the resolved pipeline without dispatching.`,
	"eqc.9": `Wave integrator: dependency-ordered merge with conflict resolution

At each wave barrier, merge the wave branches into main.

Order the bd-auto/<id> branches by the beads dependency graph, merge in that order, resolve conflicts, and require the gate to pass on the merged result. On resolution failure or gate failure, park the offending branch, reopen its issue with notes explaining the conflict, and flag it with bd human; the rest of the wave still lands.

Consider taking bd merge-slot around the merge so this stays correct if a second orchestrator ever runs concurrently. After a branch merges cleanly, remove its worktree (git worktree remove --force) so worktrees do not accumulate.

Note: worker worktrees are git-locked while their agent runs and released when it finishes, so cleanup must happen after the worker completes, not during.`,
	"eqc.6": `Hooks: Stop autonomy driver and compaction rehydration

The two hooks that make this survive long sessions.

Stop hook: armed ONLY while .beads/auto/run.json exists, so ordinary sessions in this repo are unaffected. While a run is active and the epic still has ready work, exit 2 with a precise next instruction on stderr (dispatch wave N: <ids>). This is the autonomy engine: the orchestrator physically cannot end the turn while claimable work remains. Honours the autonomy mode: auto drains the queue, wave pauses at each wave barrier, issue pauses after every issue.

SessionStart and PostCompact hooks: when a run is active, print the run state as context. This is the direct fix for the reported problem, the model forgetting its instructions after autocompact. The existing PreCompact bd prime hook stays.

Guard against a runaway loop: cap consecutive Stop-hook continuations and stop the run with a clear message if the wave number does not advance.`,
	"eqc.5": `Agent definitions: bd-worker, bd-reviewer, bd-integrator

Three subagent definitions in agents/.

bd-worker (isolation: worktree): implements exactly one issue. Protocol: bd update <id> --claim, git switch -c bd-auto/<id>, read the issue with bd show including design/acceptance/notes, implement, run the gate, commit with -m referencing the issue ID, and report a CAPPED structured footer (issue, branch, status, <=5 line summary). It closes its issue only after gate and review pass. Discovered work is filed with bd create --deps discovered-from:<id> and deferred, never worked in the current run.

bd-reviewer: read-only tools. Reviews the worker diff against the issue acceptance criteria, returns a pass/fail verdict with specific actionable notes.

bd-integrator: runs in the main checkout, merges the wave branches in dependency order, resolves conflicts, and requires the gate to pass on the merged result.

Two hard constraints go in the worker prompt: worktree-isolated agents cannot use heredocs with unquoted delimiters or brace expansion (use git commit -m and the Write tool), and the report must stay within its size budget because it is the only thing that reaches the orchestrator context.

Consider omitting Agent from bd-worker tools so workers cannot recursively spawn their own subagents and multiply token cost.`,
	"eqc.4": `Gate runner and pipeline stage executor

Execute the configured per-issue pipeline. Stages are ordered and each is either agent: <subagent-name> or run: <shell command>. The built-in stages are implement (the worker itself) and gate (the configured build/test/lint commands); everything after is user-extensible, which is how a custom code-review pipeline plugs in.

The binary owns run: stages (execute, capture exit code, capture a bounded tail of output). agent: stages are executed by the orchestrator or worker via the Agent tool, so the binary only resolves and reports them. Stage output fed back into a retry must be truncated to a fixed budget so a failing test suite cannot blow the worker context.

Expose $BD_ISSUE, $BD_BRANCH and a path to the diff as environment to run: stages.`,
	"eqc.3": `Wave planner: bd-auto plan

Compute the next wave of issues for a run.

Reads bd ready --parent <epic> --json, intersects with the run state (excluding parked, in-flight and already-done issues), caps at the configured concurrency (default 5), and emits the wave as JSON for the orchestrator to dispatch.

Prefer delegating dependency analysis to bd rather than reimplementing it: bd ready is already blocker-aware, and bd swarm validate already reports ready fronts and max parallelism. Call bd swarm validate at run start and surface its warnings (wrong dependency direction, orphans, cycles) rather than silently running on a bad DAG.`,
	"eqc.2": `Go binary skeleton: config loader and run state

Create bin/bd-auto, the single Go binary that backs both the hooks and the orchestrator helper commands.

Scope: cobra-style subcommand skeleton; parse .beads-auto.yaml (gate commands, ordered pipeline stages, concurrency, autonomy mode, retry count) with defaults for every field so a repo with no config still works; implement run start/status/stop over a state file at .beads/auto/run.json holding epic ID, wave number, dispatched issues, completed, parked, and a snapshot of the resolved config.

The run state file is the mechanism that makes a run survive autocompact, so treat its schema as the contract other issues code against. All output JSON on stdout, human text on stderr, so hooks can consume it.`,
	"eqc": `bd-auto: beads-driven subagent orchestration plugin for Claude Code

Claude Code plugin that drains a beads epic by dispatching one worktree-isolated subagent per issue, in dependency-ordered waves, with a configurable per-issue pipeline (gate, review, custom stages) and an end-of-wave integrator.

Problem: long multi-step sessions blow the context window, autocompact fires, and the model forgets its instructions even though every fact needed is already in the beads issues. Fix: each issue runs in a fresh, isolated subagent context; beads holds all durable state so the orchestrator can rehydrate after compaction; disjoint issues run in parallel.

Decisions locked (see design field): in-session subagents; worktree per worker; merge at end of wave; gate command + reviewer agent + user-extensible stages; retry once fresh then park; epic-scoped runs; concurrency 5; Go single binary; Stop hook armed only during a run.`,
	"6no": `Infra backoff is useless against a plan limit: parse the reset time the CLI already tells us

Infra backoff retries 5 times at 5s doubling, roughly 75 seconds total. A session or plan limit resets in tens of minutes: drain 6 hit "You have hit your session limit · resets 3:20pm (Europe/Berlin)" at 12:38, a 26 minute wait. Every retry was spent against a wall that had no chance of moving.

The CLI states the reset time in the result text and bd-auto discards it. At minimum parse it into the stop reason so a human knows when re-running is worth anything. Better: when the reset is known and near, wait for it rather than burning attempts; when it is far, stop the run and say so in one legible line.`,
	"r4h": `The wave table renders every row whatever the terminal height is, so a long run scrolls its own summary off the screen

internal/tui/model.go View() writes the heading, one line per issue in m.order, every barrier row, the summary, the status line and the key line, and never consults m.height(). Only the transcript view windows itself (internal/tui/detail.go bodyHeight/window). A 30-issue scope on a 20-row terminal renders 36 lines: the terminal keeps the last 20, so the heading and the first rows are gone, and there is no key to scroll them back. Adding the barrier block makes this easier to hit — a wave's barrier adds a row per branch plus a gate row plus a rule.

A fix has to window the table the way the transcript is windowed: a body height, an offset the cursor keeps in view, and something on screen saying how many rows are off it in each direction. The summary, status and key lines are the fixed chrome and must survive; they are what says whether the run is still moving.

Found by the bd-auto worker on beads-auto-imp-j5a.7, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"cx0": `'bd-auto issue run' sees none of the event stream, so its gate is as silent as the table's used to be

internal/cmds/issue.go:72 builds the engine with a Sink and no Bus, so the run-level events never reach it: it renders raw runner events through progressSink (issue.go:112) and nothing else. That means the stage boundaries added for beads-auto-imp-j5a.6 are invisible there, and so is everything else on the bus.

The effect is the one that issue fixed for the wave table: during the gate, and during any 'run:' stage, 'bd-auto issue run' prints nothing at all and looks hung, for however long 'go test ./...' takes. Its own doc comment calls it 'the thing to reach for when something is wrong', which is exactly when a silent stretch is most expensive.

A fix would give issueRun a Bus with drain.PlainRenderer(os.Stderr) attached, alongside or instead of progressSink — the two overlap, since PlainRenderer already renders activity events with their role — and would need to settle which of them owns stderr so a reader does not get every tool call twice.

Found by the bd-auto worker on beads-auto-imp-j5a.6, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"7tu": `TestConcurrentQuestionsQueueIndependently is flaky: the queue is ordered by enqueue, not by AskedAt

internal/ask/ask_test.go:225 asserts Pending()[0].AskedAt is not after Pending()[1].AskedAt, but the two orderings are set at different moments and can disagree. Broker.Ask stamps q.AskedAt = b.now() at internal/ask/ask.go:371, before it takes any lock; Broker.enqueue (ask.go:449) appends to b.order under b.mu, and Pending (ask.go:475) returns questions in b.order. Two goroutines asking at once can stamp in one order and reach the lock in the other, so Pending comes back with the later timestamp first. Reproduces on the unmodified tree at roughly 1 run in 8 under 'go test -race -count=1 -run TestConcurrentQuestionsQueueIndependently ./internal/ask/'. A fix has to pick which ordering is the contract — Pending's doc comment says oldest first — and make the other follow it: either stamp AskedAt inside enqueue under the lock, or sort Pending by AskedAt as ask.go:689 already does elsewhere.

Found by the bd-auto worker on beads-auto-imp-j5a.5, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"xhw": `Refresh the code index at the wave barrier, and measure whether it paid for itself

Stage 4 of plans/graph-index.md, and the stage that decides whether the index feature stays.

Run 'graphify update' at each wave barrier after the integrator merges, so wave N+1 navigates wave N's codebase rather than the run's base commit. The refresh costs under a second and no tokens, so there is no cost argument for accepting the staleness.

Then measure. Following scripts/resume-vs-fresh.sh, drain one fixture epic twice from the same commit, with the index enabled and disabled, and compare total_cost_usd — never summed tokens, since cache reads bill at a fraction of input price and would flatter whichever arm reads more transcript.

The measurement matters because planning showed the saving is narrower than the original idea assumed: 'graphify query' returns a truncated list of file:line and never prose, so the index can replace an agent's search phase but not its reading. The cheap exact tools (god_nodes at ~60 tokens, get_node at ~100) are where any real win should show up. Flip 'graph.enabled' to true by default only if the number supports it; this repo sets knobs from measurements, as the max_rounds comment in .beads-auto.yaml records.

Depends on the index existing, so it cannot start until stages 1-3 land.

Found by the bd-auto worker on beads-auto-imp-tv9, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"1xg": `Build the graphify code index and offer it to the roles as a tool server

Stages 1-3 of plans/graph-index.md, which beads-auto-imp-tv9 planned but deliberately did not implement.

Add an internal/graph package that writes a generated .graphifyignore and runs 'graphify extract --code-only --out .beads/auto/graph' in the main checkout, stamping the commit it was built from. Attach the resulting graph to worker, reviewer and integrator runs via a new attachGraph that mirrors attachAsk in internal/drain/ask.go, and add the prompt sections telling each role that the index is a finding aid whose every fact must be confirmed by reading the file.

Three things were verified against graphify 0.9.30 during planning and each will break a naive implementation. 'graphify.serve' imports 'mcp', which a plain 'graphify' install does not pull in, so the preflight must confirm the 'graphifyy[mcp]' extra rather than merely that graphify is on PATH. 'graphify extract' writes no 'graphify-out/.graphify_python' (only the skill's build path does), so the interpreter must be resolved from the 'graphify' binary's shebang — guessing 'python3' fails under 'uv tool install'. And the MCP server offers ten tools, three of which ('list_prs', 'get_pr_impact', 'triage_prs') reach GitHub; ToolServer.Tools must name only the seven graph tools, or a worker running behind the no-push git guard gets a route straight through it.

Ships with 'graph.enabled: false'. Everything fails open: no graphify, no extra, or a failed build means no tool, no prompt text, and a run that behaves exactly as it does today.

Found by the bd-auto worker on beads-auto-imp-tv9, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"j5a.7": `Show the barrier in the table: a merge is work, and right now it is one status line and a frozen screen

A wave barrier merges every branch, spawns a model for any conflict, runs the gate on the merged result, rolls back and blames a branch when the gate goes red, reconciles bd and files the wave's discoveries. It can take minutes and it spends real money. The table shows one line of it:

    wave 2 integrating: merging 3 branches

and then nothing changes until it is over, when that line is replaced by 'wave 2 integrated: 2 merged, 1 parked, gate failed'. Every row above it is frozen on its final state. There is no way to tell a barrier that is resolving a conflict from one that has hung.

Three separate reasons the screen is empty, all of them fixable:

1. The barrier's model never reaches the bus at all. Workers get a tagged sink from forIssue (internal/drain/drain_wave.go:472), but the barrier runs on the top-level engine, and internal/cmds/drain.go:118 builds that engine with no Sink. The conflict resolver — the one model invocation integration ever makes (internal/drain/integrate.go:507) — streams into nothing.

2. There are only two barrier events, and they are the two ends. EventWaveIntegrating (integrate.go:198) carries every branch at once, EventWaveEnd (drain_wave.go:544) carries the whole result at once, and between them is however long it takes. Nothing marks one branch starting, one branch landing, the gate running, or the rollback that follows a red gate.

3. The TUI already receives more than it shows. integrated() (internal/tui/model.go:650) folds exactly two facts out of IntegrateReport back into the rows — a parked merge and a resolved one — and drops the rest: per-branch outcome (clean, resolved, parked, skipped), the conflicted paths, per-merge Usage and Seconds, the gate's results, which branch a red gate was blamed on, what was reconciled, what was filed. The barrier's own cost is accumulated into m.barrier (model.go:179) and folded into the run total, so it is spent but never shown as a line anyone can look at.`,
	"j5a.6": `The table says 'running' where it could say which role is running: worker, reviewer, gate, integrator

An issue's row says 'running' for its whole life. It says nothing about which of the several processes an issue takes is running right now — the worker, a reviewer, the gate, the integrator, or a custom stage the repo configured.

The information is already on the wire. internal/drain/bus.go:186 tags every activity event with the role that produced it (re.Role), and internal/tui/model.go never reads e.Role at all: apply() folds EventActivity into the row's text and usage and drops the role on the floor. The single-issue plain renderer already prints it — internal/cmds/issue.go:119-128 writes '[worker]', '[reviewer]' in front of every line — so the TUI is the one view that hides it.

Roles are not a fixed list. runner.BuiltinRoles() is worker, reviewer and integrator, but an agent stage runs under whatever role the config names for it (internal/drain/issue.go:401, role = Cfg.Role(s.Agent)), so the column has to render an arbitrary configured name.

There is a second half that the event stream cannot answer, and it is the more misleading one. Non-model stages emit nothing: the built-in gate and any 'run' command stage execute without a runner, and grep confirms the only per-issue events during an attempt come from the runner's own sink (internal/drain/drain.go:450). So while the gate is running the row keeps showing the worker's last tool call with the clock still climbing — it looks like a worker that has stalled, which is precisely the reading the display exists to prevent. The same gap sits between rounds and between stages.`,
	"j5a.5": `Refill a wave's freed slot instead of holding it idle until the barrier

A wave is planned at exactly the concurrency cap and never grows. internal/wave/plan.go:75 stops filling at opt.Concurrency, and internal/drain/drain_wave.go:395 runs that fixed set behind a semaphore of the same size. The semaphore does release a slot when a worker ends — but the wave has no further issues to hand it, so the slot stays empty until every sibling finishes and the barrier has merged, gated and reviewed.

So with concurrency 3 and ten in-scope issues, one issue parking in its first minute means two workers for the rest of the wave while eight rows sit in the table saying 'waiting'. A parked issue is not doing anything; it should not be holding a slot.

What can actually be dispatched into a freed slot is whatever bd already calls ready and the scope allows, minus what the run has handled. That is the same question wave.Plan already answers — it is only ever asked once per wave.`,
	"j5a.4": `A sibling running in the same wave is never a blocker: say so in the worker prompt, and report it when a worker claims otherwise

Reported from a live run in another repo: a worker declared its issue out of scope until another issue running in the same wave finished, and stopped.

By construction that claim is false about dependencies. A wave is bd's own ready front intersected with the run's scope (internal/wave/plan.go:67, which trusts 'bd ready' precisely because bd ready is blocker-aware), so no member of a wave holds a 'blocks' edge over another member. If a worker genuinely needs another issue's output, the graph is missing that edge — and the run cannot know it, because nobody wrote it down.

Two halves to this, and neither is worth doing alone.

The prompt half: prompts/worker.md:9-11 tells the worker that other workers are running right now on other issues, and then never returns to it. Step 7 (line 91) offers 'genuinely blocked' as an option with no guidance about what does and does not count. A model reading that, on an issue whose code touches the same area as a sibling's, reasonably concludes it should wait.

The reporting half: when a worker does park itself naming a sibling, the run should say so as what it is — a missing dependency edge — rather than leaving a human to read a park reason and guess. bd-auto knows the wave's members (runstate WaveIssues), so a park reason mentioning one of them is a cheap and specific signal.`,
	"j5a.1": `Enter opens the selected issue's transcript in a Claude Code-style detail view

The wave table is one line per issue and that line is all there is. Row.Detail holds the last thing that happened and the partial-message buffer is capped at 400 runes (internal/tui/model.go:95); everything before that is dropped. A human watching a worker that has been on the same tool call for four minutes has no way to ask what it is doing.

The keymap has no enter binding today — internal/tui/model.go:257 handles up/down, k and q only, and Selected() (model.go:512) already gives the row under the cursor.

The data exists in two places:

- Live: drain.EventActivity carries Role, Tool, Text and Phase (internal/drain/bus.go). But runner.Event (internal/runner/runner.go:343) carries a tool NAME and no tool input, so 'Bash(go test ./...)' cannot be rendered from the live stream as it stands.
- On disk: every model process writes its full stream-json transcript to .beads/auto/logs/<issue>-a<attempt>-r<round>-<role>.jsonl — drain.LogPath (internal/drain/drain.go:594), set at internal/drain/issue.go:472 and :481 for the worker and each stage, and internal/drain/integrate.go:514 for the integrator. That file has everything the view wants: assistant text, tool_use blocks with their input, tool_result blocks with is_error, and the result line with usage and cost. Nothing reads these files today.

The detail view should look like a Claude Code session, not like a log dump: assistant prose as wrapped text, each tool call as a header line carrying the tool name and a one-line argument summary, its result indented underneath and truncated with a marker for what was cut, errors in the failure colour, and a separator between rounds and stages naming the role that ran.`,
	"xzx": `An unparked retry overwrites the transcript of the attempt it is retrying

Drain 2 killed kv-555.4 mid-flight, parked it, and was then told to unpark and retry. 'run unpark' resets the attempt counter — deliberately, it is a retry budget — so the retry's worker ran as attempt 1 again, and LogPath named the same file the killed attempt had written. The runner opens it with os.Create, which truncates. The killed attempt's transcript is gone: .beads/auto/logs/ has one kv-555.4-a1-r0-worker.jsonl and it is the retry's.

That transcript is the only record of what the killed worker did before it died, which is exactly what somebody investigating a kill wants to read. It is also the thing the run's own usage accounting is reconciled against, so a clobbered log makes the per-process table silently disagree with the run total.

The identity issue+attempt+round+role stops being unique the moment attempts are reset, so the path has to cope: keep the existing name when it is free and take the next free -N suffix when it is not.`,
	"dql.10": `record turns alongside cost and tokens

The claude CLI reports num_turns on its result line, bd-auto already parses that line for cost and tokens, and then drops the turn count on the floor. It is the one number that says how much work a process actually did rather than how much it cost, and it is what separates a worker that solved something in four turns from one that ground through forty for the same money. Today the only way to get it is to read .beads/auto/logs/*.jsonl by hand.`,
	"dql.7": `the wave table's last line is erased on exit, so the finished view is never seen

Two faults that meet at the same line.

bubbletea's standard renderer writes the final frame on a graceful shutdown and then, in stop(), erases the line the cursor is on — which is the last line of that frame. Verified against v1.3.10 with a three-line probe program: the third line is gone from the pane after the program exits. For this view the last line is keys(), so every drain that ends normally loses its key line off the bottom of the final frame.

And the finished branch of keys() promises 'the run is over · q to close', which is not a thing that can happen: finishedMsg sets finished and returns tea.Quit in the same Update, so no keystroke is ever handled while finished is true. The one line the user is told to press a key at is the line they never see, and the key would do nothing if they did.

Found by photographing the run-end scene in scripts/tui-shots.sh: the finished capture is the run-end capture minus its last line.`,
	"dql.6": `config show omits the ask block, so the one setting with a surprising default cannot be read back

The config file's own header says 'bd-auto config show' prints the resolved values, and it prints every block except ask. That is the block a reader most needs resolved: enabled defaults to on, roles deliberately excludes the reviewer, and hold and timeout are the two numbers that decide whether a question survives a backend's idle limit. Found while setting up the kv test lab: the lab config sets all four and config show reported none of them, so there was no way to confirm the file had been read.`,
	"dql.5": `Record usage and turns for both runs

Per-issue and per-run cost and tokens from the run's own state, plus turns per model process.`,
	"qhs": `Put a bar on what a worker files as discovered work

Worker prompt step 6 currently says 'file anything you discovered' with no threshold, and the backlog shows what that buys: 1.54 new issues per issue worked, and issues like beads-auto-imp-r4a which is a limitation the worker verified and then wrote into a code comment — filed as work that can never be completed by implementing anything.

The bar belongs in bd-auto's own worker prompt, not in beads' init template, because bd-auto is what spawns the worker and prompts/worker.md is the half of the contract bd-auto owns.

State the test as something a worker can apply: would a human schedule this as a separate piece of work? A limitation you documented where it lives is documented. A restatement of the issue you just finished is not a discovery.`,
	"txg": `Cut a refused round short instead of paying for the whole flail

Found while fixing beads-auto-imp-o4q. The CLI reports permission_denials only on its final result line, so the engine cannot act until the process has already finished. In the case that motivated o4q that was 226 seconds and 0.41 USD of a worker trying git update-index, the update-config skill and touch, all refused, before anything could notice.

o4q stops the run after that one round instead of burning the whole attempt, which is most of the win. What is still paid for is the round itself.

The denial is visible mid-stream, in the tool_result whose text is 'Claude requested permissions to write to <path>, but you havent granted it yet'. The adapter could watch for it and cancel the process on, say, the third refusal in a row with no successful tool call in between, and report the same reason. Weigh it carefully: this is string matching against a CLI message rather than the structured array, so it will rot, and killing a run on a heuristic is how a legitimately-recovering worker gets cut off. Measure how often the pattern actually appears before building on it.`,
	"84g": `Report the tools a scoped reviewer was refused

Result.Denials now carries the tools a backend refused (added by beads-auto-imp-o4q for the worker's no-progress path). Nothing reads it for a judging stage.

A reviewer runs under permissions: scoped with a fixed allowlist, so it is refused things by design and that is correct. What is missing is any report of it: if the allowlist is too narrow for a repo, the reviewer quietly returns a weaker verdict on less evidence and nobody finds out. agentStage could log the refused tool names alongside the verdict, or write them into the review notes at .beads/auto/review/<id>.md, so a repo can widen allowed_tools on evidence rather than on a guess.

Not a failure path: a scoped denial must stay non-fatal. This is reporting only.`,
	"g7t": `beads' jsonl re-import reverts bd-auto's live bd writes, not just the failure notes

Wider case left uncovered by beads-auto-imp-so5, which only made the RETRY FEEDBACK independent of bd by carrying it in run state.

The mechanism is not specific to notes. beads sets core.hooksPath=.beads/hooks; post-checkout and post-merge both run 'bd hooks run <hook>', which imports .beads/issues.jsonl over the Dolt database. Any checkout-shaped git operation therefore reverts every bd write made since the jsonl on the incoming ref was exported.

Observed live in this repo during the so5 session: a 'git pull --rebase' in the main checkout silently reverted 8 issues from closed back to open.

Confirmed in this repo, non-destructively, while fixing so5:
  - core.hooksPath is .beads/hooks and post-checkout is installed
  - a clone of this repo showed the hook invoking 'bd import' on 'git worktree add'
  - 'bd import --dry-run' of HEAD:.beads/issues.jsonl over the live DB reported 'Would import 50 issues', with so5 recorded as open in the jsonl while the DB had it in_progress

What is still exposed during a bd-auto run:
  - a worker closing its issue, then any wave barrier merge / worktree creation reverting it to open
  - BD.Park's reason and status on a parked issue
  - BD.Reset between attempts
  - the integrator's epic close
Run state keeps its own copy of done/parked/attempts, so bd-auto's control flow survives; what does not survive is the state a HUMAN reads out of bd afterwards, and any decision bd-auto re-derives from bd rather than from run state.

The hooks belong to beads, not to bd-auto, so the fix is unlikely to be 'change the hooks'. Candidates worth weighing:
  - re-assert bd-auto's own view of an issue after any operation that can fire the hook (a reconcile pass at each barrier, driven from run state)
  - set BEADS_HOOK_TIMEOUT or an env guard around bd-auto's own git operations, if beads offers one
  - export before, rather than only after, the git operations bd-auto itself runs
  - raise it upstream in beads: an import that clobbers newer rows with no warning is a data-loss default

Whatever is chosen needs the same treatment so5 got: prove it in this repo first, and make the regression test fail before it passes.`,
	"b6m": `Flaky: concurrent git worktree add fails on another worktree's commondir

Seen once in a full 'go test ./...' run, in TestKillFailsOneIssueAndLeavesTheWaveRunning:

  worktree: create .../wt/t-1: git worktree add --quiet -b bd-auto/t-1 ... :
  exit status 128: fatal: failed to read .git/worktrees/t-2/commondir: Success

It did not reproduce in 8 runs of that test alone or 5 runs of the whole drain package, before or after the change it was seen on, so it looks like a pre-existing race between one goroutine running 'git worktree add' and another removing or pruning a sibling worktree in the same repo — git re-reads every entry under .git/worktrees while adding one, and a half-removed entry is what that errno-less 'Success' message is.

If it is real it affects real runs, not only tests: a wave creates and removes worktrees concurrently. Worth reproducing under a stress loop before deciding whether internal/worktree needs its own lock around add and remove.

Discovered while implementing beads-auto-imp-908.`,
	"a70": `bd-auto handoff: open the pull request for a run that already finished

Engine.Handoff runs only from inside a drain, at finish. A run that was interrupted, or one whose parked issue a human unparked and fixed by hand, therefore has an epic branch with everything on it and no way to open the pull request short of re-running the whole drain or doing it manually with gh.

A 'bd-auto handoff [--force]' command would read the epic branch and the base out of run state, re-gate the branch, and run the same HandoffReady predicate. It is a thin wrapper over what already exists: internal/drain/handoff.go is already an exported method over a DrainReport, and the only new part is rebuilding enough of that report from run state.

Discovered while implementing beads-auto-imp-908.`,
	"1n4": `scripts/smoke.sh cannot run in the main checkout during a drain; give it an isolated mode

smoke.sh refuses to start while a run is active, correctly, because its cleanup deletes .beads/auto. That means it cannot be run at all while bd-auto is draining its own epic, which is exactly when a worker changing bd-auto needs it. Verifying wz9.11 required hand-building a throwaway repo (git init, bd init --prefix, copy bin/bd-auto and scripts/smoke.sh in) and running smoke there. That harness should be part of the repo: scripts/smoke.sh --isolated, or a make target that builds the throwaway repo and runs smoke inside it. resume-vs-fresh.sh already has most of the fixture-repo code to borrow.`,
	"6up": `Drop the plugin-era runner role aliases (bd-worker, bd-reviewer, bd-integrator)

internal/config/runners.go keeps roleAliases mapping bd-worker/bd-reviewer/bd-integrator onto worker/reviewer/integrator, so configs written when those were Claude Code subagent names keep loading. The subagent definitions themselves are gone as of wz9.11. The aliases are now pure compatibility weight and a source of confusion: 'agent: bd-reviewer' reads as if it dispatches something. Removing them is a breaking change for any config still using the old names, so it wants a version bump and a note in the README, which is why it was not done inside wz9.11.`,
	"utp": `drain: the scope prompt can swallow keystrokes meant for the live view

selectInteractively wraps os.Stdin in a bufio.Reader to read the selection and the confirmation. bufio reads ahead, so anything typed after the confirming newline sits in a buffer that is discarded when the function returns — and the live view, which takes over os.Stdin immediately afterwards, never sees it. Typing ahead on a slow start therefore loses keys, including k and q. Fix by keeping one reader for the whole command and handing its buffered remainder to tui.Options.Input, or by draining stdin before the view starts.`,
	"o4q": `permissions: auto refuses a headless worker every file write

Found while building the resume-vs-fresh measurement (wz9.7).

With runners.default.permissions: auto — the shipped default — a 'claude -p' worker in a throwaway git repo was refused every Write and every shell redirect: 'Claude requested permissions to write to <path inside its own worktree>, but you haven't granted it yet.' Headless there is nobody to grant it, so the worker looped for 226 seconds and $0.41 trying alternatives (git update-index, the update-config skill, touch), changed nothing, and the attempt died on the no-progress check.

The experiment switched to permissions: bypass and the same worker wrote files immediately.

Unclear whether this reproduces in this repo's own worktrees — the temp repo may simply not have been a trusted project — but the default is 'auto', the failure mode is a whole attempt of silent flailing at full price, and no-progress reads as 'the model did nothing' rather than 'the model was not allowed to'. Worth reproducing under .beads/auto/wt/ here before a real drain relies on it.

If it does reproduce, two things are wanted: a permission denial should classify as something other than work-failed, and the default should be one that can actually write.`,
	"so5": `beads' post-checkout hook wipes bd-auto's failure notes, so a fresh retry starts blind

Found while building the resume-vs-fresh measurement (wz9.7). Reproduced deterministically with git alone, no models involved.

A fresh attempt gets no feedback in its prompt: drain.attempt() starts each attempt with an empty feedback string, so workerPrompt renders the plain task and tells the worker to read the issue's notes. The failure notes ARE therefore the whole feedback channel for the retry path.

They do not survive. beads installs core.hooksPath=.beads/hooks, whose pre-commit EXPORTS the database to .beads/issues.jsonl and whose post-checkout IMPORTS that file back over the database. The sequence bd-auto runs is:

  1. worker commits in its worktree     -> pre-commit exports the DB to issues.jsonl on the branch
  2. the attempt fails
  3. noteFailure writes the reason to the issue  (DB only; nothing is committed)
  4. discardAttempt removes the worktree and deletes the branch, taking the exported jsonl with it
  5. worktree.Ensure creates the next attempt's worktree from base
     -> post-checkout imports the BASE jsonl over the DB, reverting everything since step 1

Step 5 deletes the note written in step 3. Verified in a throwaway repo: a note written before the worker's commit survives; a note written after it (which is where bd-auto writes) is gone the moment the next attempt's worktree is created.

Observed live first: a fresh-heavy run (max_rounds 1, retry 3) over a fixture issue burned all four attempts. The attempt-2 transcript shows the worker ran 'bd show' and got no NOTES section at all, so it repeated attempt 1's mistake exactly, three times, at full price.

Asymmetric by construction: resume rounds keep the feedback in the prompt and never re-create the worktree, so they are unaffected. Only the fresh-retry path is broken, and it fails silently — the engine's warning is discarded whenever Log is nil, which is what --quiet does.

Worth checking whether the same holds in this repo (it has the same hooks) before trusting retry to recover anything.`,
	"tpk": `smoke.sh: cover bd-auto drain end to end with provider: fake

scripts/smoke.sh still exercises only the plugin-era path (run start, plan --dispatch, worker done, merge-order). The headless drain has unit coverage but no cross-process smoke test.

Extend smoke.sh with a drain case: a throwaway epic of three trivial issues, runners.default.provider set to fake, an explicit --issues scope, and assertions that every branch merges, the epic closes, and the non-TTY refusal (bare --epic with no scope) exits non-zero having dispatched nothing.

Discovered while implementing wz9.9. The plan (plans/headless.md, Verification, Manual item 1) names this; it was left out because the acceptance criteria for wz9.9 are all covered by go test.`,
	"46o": `cmds/merge.go duplicates the integrator's candidate gathering

internal/cmds/merge.go has its own currentBranch, commitsAhead, branchExists and listWorktrees, which internal/drain/integrate.go now also has (over internal/worktree.List). Once the plugin surface is cut (beads-auto-imp-wz9.11), merge-order should either go or be rebuilt on Engine.candidates so there is one place that decides what a mergeable candidate is.

Discovered while implementing beads-auto-imp-wz9.8.`,
	"vmb": `Integrator: re-merge branches peeled back with a gate offender

internal/drain/integrate.go blameGate peels merges back newest first to find which branch a red wave gate is about. Every branch peeled off after the offender is parked too, even though nothing is wrong with it: the work is intact on its own branch, it just is not in the merged result.

A better barrier would, once the offender is identified, re-merge the branches that were peeled with it and gate once more, parking only the offender. Cost is one extra gate run in an already-failing path.

Discovered while implementing beads-auto-imp-wz9.8.`,
	"gvg": `bd-auto issue run creates a statusless run.json that run status reports as active

The drain engine writes sessions and attempt counters through runstate.Update(create: true), so a standalone 'bd-auto issue run' with no drain around it creates .beads/auto/run.json. The created state has an empty Status, which is deliberate: runstate.Active() is false for it, so no hook is armed. But 'bd-auto run status' only special-cases ErrNoRun, so it then prints active: true with an empty epic and wave 0.

Either give run status a check for a run with no epic, or have the engine record standalone runs under a distinct status. Cosmetic, but it is the first thing anyone debugging a single issue will see.`,
	"xdv": `Worker commits pick up .beads/issues.jsonl churn from other workers

gitguard chains beads' own pre-commit hook into every worker worktree, which is correct: it keeps .beads/issues.jsonl in sync. But issues.jsonl is a tracked file exporting the SHARED dolt database, so a worker committing in its worktree stages an export reflecting every other worker's bd writes as well as its own. At concurrency 5, every branch in a wave carries a different full snapshot of the same file, and the integrator hits a conflict on it for every merge after the first.

Options: exclude .beads/issues.jsonl from worker commits via the generated rejector dir (a chained pre-commit that unstages it), have the integrator resolve that path with a regeneration rather than a merge, or drop the chain for pre-commit specifically inside worker worktrees. Verified against this repo: bd's pre-commit exported 37 issues during a normal commit here.`,
	"vz7": `Preflight the claude CLI before a drain spends anything

internal/runner/claude builds argv against flags verified present in CLI 2.1.233 (--append-system-prompt, --permission-mode auto, --include-partial-messages, --session-id/--resume, --allowed-tools). An older or newer CLI that dropped or renamed one fails per-spawn, and every worker in the wave hits it separately: the failure has no result line, so it classifies as infra-failed and the engine backs off and retries an unrunnable command. A one-shot preflight at drain start -- claude --version plus a trivial -p run through the same argv builder -- would turn that into one legible error before any worktree is created. Fits alongside the scope preview in wz9.9.`,
	"jf6": `config: validate provider: names a registered runner adapter

A typo in a runners: entry's provider: field loads fine and only fails when the engine is about to spawn a model, which is after worktrees have been created and a wave dispatched. runner.New already reports the known providers; internal/config could import internal/runner/providers and check provider: at load time the same way it checks a stage's agent: against the role list. Left out of wz9.2 because it changes config load behaviour, which wz9.1 owns and tested.`,
	"r4a": `gitguard: a repo-configured pushurl survives the worktree block

internal/gitguard blocks pushes by setting remote.<name>.pushurl in the worktree config. pushurl is multi-valued and values accumulate across config files, so in a repo that already sets its own pushurl git pushes to that one FIRST and only then fails on bd-auto's. The pre-push hook covers this unless the worker also passes --no-verify, and gitguard.Verify's remote-moved predicate catches it after the fact. Verified against git 2.55; documented at the blockedPushScheme const. Options if this ever bites: refuse Setup when a remote already has a pushurl, or set protocol.allow=never in the worktree config (which also blocks fetch).`,
	"pzi": `Drop the plugin-era runner role aliases

internal/config/runners.go carries roleAliases mapping bd-worker/bd-reviewer/bd-integrator onto the worker/reviewer/integrator roles, so configs written for the Claude Code plugin (including this repo's own .beads-auto.yaml, which still says agent: bd-reviewer) keep loading while the plugin is being cut. Once the plugin is gone (beads-auto-imp-wz9 issue 11), delete roleAliases with Config.Role's alias branch and its tests, and change .beads-auto.yaml to agent: reviewer.`,
	"wz9.11": `Cut the plugin surface

Deliberately last: everything it deletes stays working until the replacement is
proven.

Delete:
- hooks/ and agents/
- internal/cmds/hook.go and its tests
- the Continuations / maxContinuations autonomy engine (hook.go:25)
- Bindings from run state
- autonomy: issue

Keep .claude-plugin/plugin.json. It costs nothing and keeps the skill
installable rather than a file users copy to .claude/skills/bd-auto/SKILL.md.

Rewrite SKILL.md as a launcher: it must name its scope (--issues or --all,
never a bare epic off a TTY), run bd-auto drain --plain in the background, and
poll bd-auto run status. This is where the whole project's acceptance criterion
is actually met, so measure it here: the launching session must grow by under 2k
tokens regardless of epic size.

Update README.md and .beads-auto.yaml to the new config shape, including the
runners: block, the agent:-names-a-runner-role change, and the removal of
autonomy: issue.`,
	"wz9.10": `TUI: live wave view with working stop controls

bubbletea + lipgloss wave table over the event stream from the drain engine:
live per-worker activity (text-granular, which is what --include-partial-messages
buys), accumulated cost per issue and for the run, and wave/attempt state.

Plus the control channel, which ships in the same issue because it is the only
way to abandon a run in flight: k kills the selected worker, q / ctrl-c stops
the run with state intact. Both depend on the process-group kill from the claude
adapter; killing the direct child only would leave a go test running and its
worktree held.

Cost is displayed, never enforced. Nothing here is a budget. With scope chosen
up front the run is already bounded; this is so you can watch it and change your
mind.

Non-TTY falls back to the plain renderer from the drain issue.`,
	"wz9.8": `Go-driven integrator and a scope-aware epicComplete

Merging becomes deterministic Go. A model is spawned only where judgement is
genuinely required, which is a real merge conflict:

  for _, b := range mergeOrder {
      if err := gitMerge(b); err != nil {
          res := runner.Run(ctx, conflictRequest(b), sink)   // model ONLY here
          if !resolved { park(b); continue }
      }
  }
  pipeline.Gate(...)      // Go
  cleanupWorktrees()      // Go
  if epicComplete(st) { bd.Close(epic) }   // Go

epicComplete is the four-condition predicate currently written as prose at
bd-integrator.md:56-64. It is a pure function of run state, so it becomes a
tested Go function rather than something a model is asked to get right.

It now also has to reason about scope: a run whose scope was a subset of the
epic finishes with children still open, and must leave the epic alone.`,
	"wz9.4": `internal/wave: extract plan and merge-order logic out of cmds

Pure refactor, no behaviour change. Lift the wave-planning logic from
cmds/plan.go and the merge ordering (topoOrder, merge.go:105) from cmds/merge.go
into callable functions returning structs, so the drain engine can use them
without shelling out to itself. The CLI commands become thin wrappers.

Doing this as its own issue keeps the behaviour-changing work downstream honest:
if the existing tests need editing, something changed that should not have.`,
	"wz9.3": `prompts/: rewrite the three role prompts for headless execution

agents/*.md are subagent definitions, not role prompts, and converting them is a
rewrite rather than a frontmatter strip.

bd-worker.md alone carries: the BD-AUTO: footer protocol (hook.go:215, being
deleted), "your report is the only thing that reaches the orchestrator" and its
25-line cap (there is no orchestrator context left to protect), "a hook blocks
those anyway" (there is no hook), and a Shell section about Claude Code refusing
statically unverifiable commands under worktree isolation, which does not apply
to a plain claude -p in a plain worktree.

Every role prompt also gains an explicit override of the repo's own CLAUDE.md,
which says "Work is NOT complete until git push succeeds... NEVER stop before
pushing." A headless claude -p loads CLAUDE.md exactly like an interactive
session. Today the PreToolUse deny answers that with an actionable message;
afterwards the worker gets a bare git hook rejection and, told push is
mandatory, retries or reaches for git pull --rebase, which is the rebase hole
gitguard has to close. The prompt says in as many words: you are one attempt
inside a bd-auto run, you commit to your own branch and stop, the integrator
merges, nothing here pushes.

Early on purpose: issues from the pipeline onward cannot spawn anything without
these.`,
	"wnu": `Auto-generate .beads-auto.yaml on run start when absent

A repo with no .beads-auto.yaml runs on built-in defaults today, which works but leaves the user nothing to edit. On bd-auto run start, write the starter template to the repo root when no config file exists, then load it so the run uses it. Confine this to run start: hooks fire constantly and from inside worktrees, so generating there would be surprising.`,
	"032": `bd-auto init: generate a starter .beads-auto.yaml

Add a bd-auto init command that writes a commented .beads-auto.yaml into the user's working directory. Refuses to clobber an existing file unless --force. Template values must be derived from the config package defaults so the documentation cannot drift from the constants.`,
	"v5q": `Raise default review max_rounds from 2 to 3

The dogfood run on beads-auto-imp-eqc.13 needed three review rounds to settle the PreToolUse merge guard: each fix opened the opposite hole. max_rounds=2 would have parked it. Raise DefaultMaxRounds to 3 and update the config file, README and SKILL.md text that quote the old default.`,
	"tnd": `Multi-worker dogfood: watch a 3-4 issue parallel wave with a real merge conflict

The eqc.13 dogfood run drained a real epic with the plugin loaded, but wave 1 was a single issue, so the parallel path was never watched live. Deterministic coverage exists in make smoke; agent behaviour under contention does not.

Set up a throwaway epic of 3-4 genuinely independent issues, at least two of which touch the same file so the integrator meets a real conflict. Run with --autonomy wave so there is a barrier to inspect.

Watch for: several workers holding worktrees at once, the reviewer's send-back loop retaining worker context, the integrator resolving a genuine conflict, retry-then-park end to end, and whether the Stop hook holds the orchestrator on task across a real autocompact.

Measure: orchestrator context growth per issue (the number eqc.13 could not observe from inside a worker), and wall clock against a sequential baseline. Fold the numbers into the README's dogfood section, which currently states plainly that these are unproven.`,
	"eqc.13": `Dogfood run: drain a real epic with the plugin loaded

Point the finished tool at a real epic in this repo and let it drain, with the author watching. Record what actually happened: orchestrator context growth per issue, wall-clock versus a sequential baseline, how many issues needed a retry, how many merges conflicted.

Write the README from that run rather than from the design: install, config reference for .beads-auto.yaml, how to add a custom pipeline stage, how to stop a run, and how to recover a parked issue.

This epic is itself the natural first target once enough of the pipeline exists.`,
	"eqc.8": `PreToolUse hook: agent-to-issue binding and merge guard

PreToolUse hooks fire inside subagents and carry agent_id and agent_type. Match on Bash and inspect the command.

Binding: when a worker runs bd update <id> --claim or bd ready --claim, record agent_id -> issue in the run state. That mapping is what lets SubagentStop verify the right issue and what would later enable per-worker file-ownership rules.

Merge guard: deny a worker any git merge, rebase onto main, or push. Only the integrator merges, and only at the wave barrier. Denying this deterministically is far more reliable than asking the worker not to.`,
	"eqc.7": `SubagentStop verification hook

When a worker finishes, independently verify it actually reached a terminal state rather than trusting its report: the issue is closed, or blocked with notes, and a bd-auto/<id> branch exists with a commit. If not, exit 2 to keep the worker running with a precise reason.

This closes the gap where an agent announces success without having done the bookkeeping. Depends on knowing which issue the finishing agent owned, which is what the PreToolUse claim-binding issue provides; fall back to parsing the required report footer when no binding exists.`,
	"vzz": `bd-auto worker status reports any run.json on disk as active

internal/cmds/worker.go:216 returns a hard-coded "active": true for every state that loads, special-casing only runstate.ErrNoRun — the same misreport beads-auto-imp-gvg fixed in 'bd-auto run status'. Against a standalone or stopped run it prints {"active": true, "in_flight": null, "wave": 0}, verified against a state file with status "standalone" and no in-flight entries. runstate.State.Active() now exists and answers this exactly; the fix is to report st.Active() there as run status does, so the two commands cannot disagree about whether a run is armed.

Found by the bd-auto worker on beads-auto-imp-gvg, and filed at the wave barrier — deferred, so it waits for a human rather than being offered to the next run.`,
	"dql.11": `the README's wave table shows a cost for a running worker, which the shipped backend cannot report

README's 'Watching a run' example gives running rows a cost — t-2 running at $0.4210 — and says the table shows 'what it has cost so far'. The claude CLI reports cost only on its result line, so a row's cost is a dash for the whole of its first model process and only appears once that process ends. Watched live on the kv-ctf drain: kv-ctf.1 showed '-' for its entire 1m45s implement stage and $0.7884 the instant it finished.

Nothing is wrong with the display — it is the honest rendering of a backend that has reported nothing yet, and the number does then accumulate across an issue's later stages, so a row in review shows what its implement stage cost. What is wrong is the example, which promises a live figure and is the first thing anyone reads about the table.`,
	"ql2": `Configure the beads Dolt remote for cross-machine issue sync

A git remote now exists (github.com/Sean-McConnachie/beads-auto-imp), but bd has its own separate Dolt remote which is not configured, so 'bd dolt push' is a no-op with setup instructions.

Issue data is not currently at risk: the pre-commit hook exports .beads/issues.jsonl, which is tracked and pushed with the code. This is about live cross-machine sync, not backup.

To set up:
  bd dolt remote add origin git+ssh://git@github.com/Sean-McConnachie/beads-auto-imp.git
  bd dolt push

Left to the repo owner because it needs an SSH-based URL and their credentials.`,
}
