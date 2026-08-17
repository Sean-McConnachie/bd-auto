package cmds

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"bd-auto/internal/ask"
	"bd-auto/internal/drain"
	"bd-auto/internal/runstate"
)

// Ask implements `bd-auto ask`: the MCP server a drain hands its own workers.
//
// It is not a command anyone types. A drain writes it into the --mcp-config it
// spawns each worker with, so the backend starts it as that worker's tool
// server; the flags fix which issue and role it may ask as, which is what stops
// a worker asking as somebody else.
//
// Everything it does is forwarded to the drain over the socket. It holds no
// state, makes no decisions, and prints nothing to stdout that is not protocol
// — stdout is the wire.
func Ask(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	socket := fs.String("socket", "", "the drain's question socket (required)")
	issue := fs.String("issue", "", "the issue this server may ask as (required)")
	role := fs.String("role", "", "the role asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return errors.New("--socket is required; `bd-auto ask` is started by a drain, not by hand")
	}
	if *issue == "" {
		return errors.New("--issue is required; `bd-auto ask` is started by a drain, not by hand")
	}

	// The backend closes stdin when the worker is done, which is what normally
	// ends this. The signal handler is for the case where the whole process
	// group is signalled instead.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shim := ask.Shim{Socket: *socket, Issue: *issue, Role: *role}
	return shim.Serve(ctx, os.Stdin, os.Stdout)
}

// startAsk opens the question channel for a run, or reports why there is none.
//
// attended says whether a human is watching this run: the wave table is up, and
// the keys it reads are how an answer gets back. Everything else — --quiet,
// --plain, --json, a redirected stream, CI — is unattended, and an unattended
// broker answers on the spot rather than parking a worker against a terminal
// nobody is looking at.
func startAsk(c *Ctx, attended bool) (*ask.Server, error) {
	if !c.Cfg.AskEnabled() {
		return nil, nil
	}
	policy := ask.PolicyUnattended
	if attended {
		policy = ask.PolicyAsk
	}
	b := ask.NewBroker(policy)
	b.Timeout = c.Cfg.AskTimeout()
	b.Hold = c.Cfg.AskHold()
	return ask.Listen(runstate.Dir(c.RepoRoot), b)
}

// openAsk is startAsk with the failure already handled.
//
// A socket that will not open is reported and stepped over rather than
// returned. The tool is worth having and it is not worth a run: without it a
// worker decides for itself, which is what every run did before it existed.
//
// The caller attaches the result to the engine and, once it has a bus, calls
// drain.WireAsk. The two steps are separate because a watched run's bus is
// built around the view, and the view needs the broker to answer with.
func openAsk(c *Ctx, eng *drain.Engine, attended bool) *ask.Server {
	srv, err := startAsk(c, attended)
	if err != nil {
		info("warning: no question channel for this run (%v); workers will decide for themselves", err)
		return nil
	}
	if srv == nil {
		return nil
	}
	eng.Ask = srv
	return srv
}

// responder is the view's end of a question channel, tolerating the channel not
// existing: an interface holding a nil pointer is not nil, and the view's
// read-only mode is exactly that check.
func responder(srv *ask.Server) ask.Responder {
	if srv == nil {
		return nil
	}
	return srv.Broker()
}

// broker is the same tolerance for the wiring call.
func broker(srv *ask.Server) *ask.Broker {
	if srv == nil {
		return nil
	}
	return srv.Broker()
}
