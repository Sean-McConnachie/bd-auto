//go:build linux

package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"bd-auto/internal/drain"
)

// Everything else in this package tests the model, which is where the
// behaviour lives. This tests the one thing a model test cannot: that the view
// works on a real terminal, in raw mode, with keys arriving as bytes.
//
// It is worth its weight because that is the only configuration the controls
// are ever used in. A view whose keys are unreachable is a run that cannot be
// abandoned — and every other test in this file would still pass.
func TestTheControlsWorkOnARealTerminal(t *testing.T) {
	control := newPressed("t-1")
	pty, tty := openPTY(t)

	ui := New(Options{Control: control, Output: tty, Input: tty})
	ui.Observe(drain.Event{Kind: drain.EventRunStart, At: time.Now(), Text: "epic-1", Issues: []string{"t-1"}})
	ui.Observe(drain.Event{Kind: drain.EventIssueStart, At: time.Now(), Wave: 1, Issue: "t-1"})

	screen := &safeBuffer{}
	go io.Copy(screen, pty)

	done := make(chan error, 1)
	go func() { done <- ui.Run(context.Background()) }()

	waitFor(t, "the table never reached the terminal", func() bool {
		return strings.Contains(screen.String(), "t-1")
	})

	// k, typed rather than injected.
	if _, err := pty.WriteString("k"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "k did not reach the control channel", func() bool {
		return len(control.kills()) == 1 && control.kills()[0] == "t-1"
	})

	// q stops the run and keeps the view up; only the second one leaves.
	if _, err := pty.WriteString("q"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "q did not stop the run", func() bool { return control.stopped() == 1 })
	select {
	case <-done:
		t.Fatal("the first q left the view; stopping is not instant and the table has to outlast it")
	case <-time.After(250 * time.Millisecond):
	}

	if _, err := pty.WriteString("q"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second q did not close the view")
	}
	if !ui.Stopped() {
		t.Fatal("the run ended on a keystroke and must be reported as stopped")
	}
}

// openPTY returns the two ends of a fresh pseudo-terminal. Done with plain
// ioctls rather than a dependency: three constants is cheaper than a module,
// and this is the only place in the repo that needs one.
func openPTY(t *testing.T) (pty, tty *os.File) {
	t.Helper()
	const (
		tiocGPTN   = 0x80045430 // read the pty's number
		tiocSPTLCK = 0x40045431 // lock/unlock the slave side
	)
	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals available here: %v", err)
	}
	t.Cleanup(func() { pty.Close() })

	var unlock int32
	if err := ioctl(pty, tiocSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		t.Fatalf("unlock the pty: %v", err)
	}
	var n int32
	if err := ioctl(pty, tiocGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		t.Fatalf("name the pty: %v", err)
	}
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open the pty's other end: %v", err)
	}
	t.Cleanup(func() { tty.Close() })
	return pty, tty
}

func ioctl(f *os.File, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); errno != 0 {
		return errno
	}
	return nil
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// safeBuffer is a bytes.Buffer the reader goroutine and the test can share.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
