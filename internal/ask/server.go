package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bd-auto/internal/runner"
)

// The transport is a unix socket, and the reason is that a backend starts its
// own MCP servers.
//
// bd-auto cannot hand a running `claude -p` a server that lives inside the
// drain process: the CLI spawns stdio servers itself, as children of the worker
// it is running. So the drain listens on a socket, and each worker's MCP server
// is a second bd-auto process — `bd-auto ask` — that speaks MCP on its stdio
// and forwards each call down the socket to the drain that spawned everything.
//
// It is one shim process per worker and one broker per run. Making the shim
// per-worker is what scopes it: the issue and role are fixed in the argv the
// drain generated, so a worker cannot ask as somebody else, and a role the
// config did not give the tool to never gets a server at all.

// SocketName is the socket the drain listens on, under .beads/auto/. It carries
// the pid so a second bd-auto process in the same repo cannot inherit a stale
// socket from the first.
func SocketName(pid int) string { return fmt.Sprintf("ask-%d.sock", pid) }

// Server is the drain's end of the channel: a unix socket in front of a Broker.
type Server struct {
	broker *Broker
	ln     net.Listener
	path   string
	bin    string
	tmp    string // the directory to remove on Close, when we made one

	wg   sync.WaitGroup
	once sync.Once
}

// Listen opens the socket for a run.
//
// dir is normally the run-state directory. A socket path has a hard length
// limit — 108 bytes on Linux, 104 on macOS — that a deep checkout can exceed,
// so a failure to bind there falls back to a temporary directory rather than
// taking the whole run down: the tool is worth having in a deep checkout too.
func Listen(dir string, b *Broker) (*Server, error) {
	if b == nil {
		return nil, errors.New("ask: a broker is required")
	}
	s := &Server{broker: b, bin: binary()}

	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			path := filepath.Join(dir, SocketName(os.Getpid()))
			_ = os.Remove(path)
			if ln, err := net.Listen("unix", path); err == nil {
				s.ln, s.path = ln, path
			}
		}
	}
	if s.ln == nil {
		tmp, err := os.MkdirTemp("", "bd-auto-ask")
		if err != nil {
			return nil, fmt.Errorf("ask: socket directory: %w", err)
		}
		path := filepath.Join(tmp, SocketName(os.Getpid()))
		ln, err := net.Listen("unix", path)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return nil, fmt.Errorf("ask: listen: %w", err)
		}
		s.ln, s.path, s.tmp = ln, path, tmp
	}

	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Broker is the question channel behind this server.
func (s *Server) Broker() *Broker { return s.broker }

// Path is the socket workers connect to.
func (s *Server) Path() string { return s.path }

// Close stops accepting, settles every open question and removes the socket.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		err = s.ln.Close()
		s.broker.Close()
		s.wg.Wait()
		_ = os.Remove(s.path)
		if s.tmp != "" {
			_ = os.RemoveAll(s.tmp)
		}
	})
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// wireCall is one request from a shim: a question, or a poll for one already
// asked.
type wireCall struct {
	Op       string   `json:"op"`
	Issue    string   `json:"issue,omitempty"`
	Role     string   `json:"role,omitempty"`
	Header   string   `json:"header,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []Option `json:"options,omitempty"`
	Ticket   string   `json:"ticket,omitempty"`
}

// The wire operations.
const (
	opAsk  = "ask"
	opWait = "wait"
)

// wireReply is what goes back. Ticket and Answer are exclusive: a settled
// question carries an answer, an open one carries the ticket to come back for.
type wireReply struct {
	Ticket  string `json:"ticket,omitempty"`
	Answer  string `json:"answer,omitempty"`
	Source  Source `json:"source,omitempty"`
	Settled bool   `json:"settled"`
	Hold    string `json:"hold,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handle serves one call. A connection carries exactly one, so a poll is a
// fresh connection: between two polls there is nothing held open, which is what
// makes a two-hour wait survivable at both ends.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()

	var call wireCall
	if err := json.NewDecoder(conn).Decode(&call); err != nil {
		return
	}

	// The asker going away mid-hold cancels this call, so a killed worker does
	// not leave a goroutine parked for the rest of the hold. It does not settle
	// the question: the worker may be about to poll again from a new process.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()

	reply, err := s.answer(ctx, call)
	out := wireReply{Hold: s.broker.hold().String()}
	switch {
	case err != nil:
		out.Error = err.Error()
	case reply.Settled:
		out.Settled, out.Answer, out.Source = true, reply.Answer.Text, reply.Answer.Source
	default:
		out.Ticket = reply.Question.ID
	}
	_ = json.NewEncoder(conn).Encode(out)
}

func (s *Server) answer(ctx context.Context, c wireCall) (Reply, error) {
	switch c.Op {
	case opWait:
		if c.Ticket == "" {
			return Reply{}, errors.New("ask: a ticket is required")
		}
		return s.broker.Wait(ctx, c.Ticket)
	case opAsk, "":
		return s.broker.Ask(ctx, Question{
			Issue:   c.Issue,
			Role:    c.Role,
			Header:  c.Header,
			Text:    c.Question,
			Options: c.Options,
		})
	}
	return Reply{}, fmt.Errorf("ask: unknown operation %q", c.Op)
}

// Spec is the tool server the engine offers one worker: this binary, run as the
// shim, pointed at this socket and fixed to one issue and role.
func (s *Server) Spec(issue, role string) runner.ToolServer {
	return runner.ToolServer{
		Name:     ServerName,
		Command:  s.bin,
		Args:     []string{"ask", "--socket", s.path, "--issue", issue, "--role", role},
		Tools:    Tools(),
		Required: true,
		// What the backend must allow one call, which is the hold plus room for
		// the round trip. It is not how long a question waits — that is the
		// broker's Timeout, and it is spent over as many calls as it takes.
		Timeout: s.broker.hold() + time.Minute,
	}
}

// binary is the bd-auto executable to re-run as the shim. The resolved path is
// used where it can be found, because a worker's PATH is not this process's.
func binary() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	return "bd-auto"
}

// --- the shim's end ---

// DialTimeout bounds connecting to the drain. Connecting is instant or it is
// never going to work: the drain is on the same machine, and the realistic
// failure is that it has already exited.
const DialTimeout = 10 * time.Second

// query sends one call down the socket and waits for its reply. It carries no
// deadline of its own — waiting is the point — so what ends it is the drain
// answering, the drain exiting, or the shim being killed with its worker.
func query(ctx context.Context, socket string, c wireCall) (wireReply, error) {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return wireReply{}, fmt.Errorf("ask: no run is listening on %s: %w", socket, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(c); err != nil {
		return wireReply{}, fmt.Errorf("ask: send: %w", err)
	}
	var reply wireReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return wireReply{}, fmt.Errorf("ask: the run stopped answering: %w", err)
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}
