// Package tui is the live view of a drain: a bubbletea wave table over the
// engine's event stream, and the control channel that is the only way to
// abandon a run in flight.
//
// It is one of three renderers on the same bus, and deliberately the least
// privileged of them: it reads drain.Events like the plain and JSON renderers
// do, and the only thing it can do that they cannot is press drain.Control. So
// a run with no terminal loses the table and nothing else — every fact the
// table shows is a fact the plain renderer prints.
//
// Off a terminal this package is never entered. cmds/drain decides that once,
// before anything is spawned, which is what keeps a redirected run from
// negotiating with a terminal that is not there.
package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
)

// Options configures a UI.
type Options struct {
	// Control is the run's stop switch — in practice a *drain.Control. Nil
	// makes the view read-only, so pass nothing rather than a nil pointer: an
	// interface holding one is not nil, and the read-only mode is exactly that
	// check.
	Control Stopper
	// Ask is where an answer to a worker's question goes back — in practice an
	// *ask.Broker. Nil shows questions without being able to answer them, which
	// is the honest thing for a view that has no channel to the run.
	Ask ask.Responder
	// Output is where the table draws. Nil means os.Stderr, which is what keeps
	// stdout clean for the final report a caller parses.
	Output io.Writer
	// Input is where keys come from. Nil means os.Stdin.
	Input io.Reader
}

// UI is a running wave table. It implements drain.Observer, so attaching it to
// a run is the same one-line operation as attaching any other renderer.
type UI struct {
	model *Model
	out   io.Writer
	in    io.Reader
	q     *queue

	once sync.Once
}

// New builds a UI. Nothing is drawn and no terminal is touched until Run.
func New(opts Options) *UI {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	in := opts.Input
	if in == nil {
		in = os.Stdin
	}
	m := NewModel(opts.Control)
	m.Ask = opts.Ask
	return &UI{model: m, out: out, in: in, q: newQueue()}
}

// Observe implements drain.Observer.
//
// It must never block: the bus delivers under its own lock, so a renderer that
// waits for a terminal stops every worker in the run. The queue is unbounded
// for the same reason it is not lossy — a wave table that has dropped an
// issue-end event disagrees with the report it is printed above.
func (u *UI) Observe(e drain.Event) { u.q.push(e) }

// Finish tells the view the run is over. The program renders its last frame and
// exits; Run returns after that.
func (u *UI) Finish() { u.once.Do(u.q.close) }

// Run draws the table until the run finishes or the human leaves. It blocks,
// and it is the only method that touches the terminal.
func (u *UI) Run(ctx context.Context) error {
	// The program gets a context of its own, cancelled the moment Run returns.
	// bubbletea's Send is a synchronous channel write that only gives up when
	// that context is done, so without this a program that failed to start
	// would leave the pump blocked on it forever.
	progCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prog := tea.NewProgram(u.model,
		tea.WithContext(progCtx),
		tea.WithOutput(u.out),
		tea.WithInput(u.in),
		// The default signal handler would quit the program behind the model's
		// back, restoring the terminal while five workers were still running.
		// Ctrl-C arrives here as a key, and a real SIGTERM is already handled by
		// the command's own signal context.
		tea.WithoutSignalHandler(),
	)
	go u.pump(progCtx, prog)

	// A cancelled caller context is a SIGINT or a SIGTERM that has already
	// reached the engine. The view follows the run out rather than outliving it.
	stop := context.AfterFunc(ctx, u.Finish)
	defer stop()

	_, err := prog.Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	return nil
}

// Stopped reports whether the human asked the run to stop from the view. The
// command reads it to explain a run that ended on a keystroke rather than on
// its own terms.
func (u *UI) Stopped() bool { return u.model.stopping }

// pump forwards queued events into the program, in order, and closes it once
// the run is over.
func (u *UI) pump(ctx context.Context, prog *tea.Program) {
	for {
		events, open := u.q.take()
		for _, e := range events {
			if ctx.Err() != nil {
				return
			}
			prog.Send(eventMsg(e))
		}
		if !open {
			prog.Send(finishedMsg{})
			return
		}
	}
}

// queue is an unbounded, ordered hand-off from the engine's goroutines to the
// bubbletea program.
//
// bubbletea's own Send is a synchronous channel write, so pushing events
// straight into it would couple every worker's progress to the terminal's
// redraw rate — and would deadlock outright for any event emitted before the
// program started running, which the first two always are.
type queue struct {
	mu     sync.Mutex
	items  []drain.Event
	closed bool
	wake   chan struct{}
}

func newQueue() *queue { return &queue{wake: make(chan struct{}, 1)} }

func (q *queue) push(e drain.Event) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.signal()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take blocks until there is something to deliver, and reports whether more may
// follow. Everything already queued is handed back even after close, so a run
// that ends in the same instant as its last event still shows it.
func (q *queue) take() ([]drain.Event, bool) {
	for {
		q.mu.Lock()
		items, closed := q.items, q.closed
		q.items = nil
		q.mu.Unlock()
		if len(items) > 0 || closed {
			return items, !closed
		}
		<-q.wake
	}
}
