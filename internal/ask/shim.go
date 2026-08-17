package ask

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// Shim is the MCP server a worker's backend spawns: `bd-auto ask`, fixed to one
// issue and role by the argv the drain generated.
//
// It holds no state and makes no decisions. Every call is forwarded to the
// drain over the socket, which is where the broker, the queue and the human
// are — so a shim that is killed with its worker loses nothing, and a question
// survives the process that asked it.
type Shim struct {
	// Socket is the drain's listening socket.
	Socket string
	// Issue and Role are who this shim may ask as. They come from the argv
	// rather than from the model, which is what stops a worker asking as
	// somebody else.
	Issue string
	Role  string
	// Hold is only used to word the PENDING instruction when the drain does not
	// say what its own hold is. Zero means DefaultHold.
	Hold time.Duration
}

// Serve speaks MCP on in and out until in closes.
func (s Shim) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Socket == "" {
		return errors.New("ask: --socket is required")
	}
	if s.Issue == "" {
		return errors.New("ask: --issue is required")
	}
	return ServeMCP(ctx, in, out, s.Handle)
}

// Handle answers one tool call by forwarding it to the drain.
func (s Shim) Handle(ctx context.Context, c Call) (string, error) {
	var call wireCall
	switch c.Tool {
	case ToolWait:
		if c.Ticket == "" {
			return "", errors.New("ask: ticket is required; it is the value " + ToolAsk + " returned")
		}
		call = wireCall{Op: opWait, Ticket: c.Ticket}
	default:
		if strings.TrimSpace(c.Question) == "" {
			return "", errors.New("ask: question is required")
		}
		call = wireCall{
			Op:       opAsk,
			Issue:    s.Issue,
			Role:     s.Role,
			Header:   c.Header,
			Question: c.Question,
			Options:  c.Options,
		}
	}

	reply, err := query(ctx, s.Socket, call)
	if err != nil {
		// The drain has gone: there is no human at the end of this and there
		// never will be, so the model is told to get on with it rather than
		// left holding a tool error it cannot act on.
		if reply.Error == "" {
			return UnknownTicketText, nil
		}
		return "", err
	}
	if reply.Settled {
		return reply.Answer, nil
	}
	return PendingText(reply.Ticket, s.holdFrom(reply)), nil
}

// holdFrom prefers the drain's own hold, so the instruction the model reads
// says how long the next poll will actually block.
func (s Shim) holdFrom(r wireReply) time.Duration {
	if d, err := time.ParseDuration(r.Hold); err == nil && d > 0 {
		return d
	}
	if s.Hold > 0 {
		return s.Hold
	}
	return DefaultHold
}
