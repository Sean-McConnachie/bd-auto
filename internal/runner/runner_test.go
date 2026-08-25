package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// resetRegistry gives a test the registry to itself, since Register panics on a
// duplicate name by design.
func resetRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = map[string]Factory{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

type stubRunner struct {
	name string
	spec Spec
}

func (s *stubRunner) Name() string       { return s.name }
func (s *stubRunner) Caps() Capabilities { return Capabilities{Resume: true} }
func (s *stubRunner) Run(context.Context, Request, EventSink) (Result, error) {
	return Result{Class: ClassOK}, nil
}

// The budget rules are the reason Class exists, so they are asserted rather
// than left to whoever writes the engine loop.
func TestClassBudgets(t *testing.T) {
	cases := []struct {
		class       Class
		valid       bool
		counts      bool
		recoverable bool
	}{
		{ClassOK, true, true, false},
		{ClassWorkFailed, true, true, false},
		{ClassInfraFailed, true, false, true},
		{ClassInterrupted, true, false, false},
		{Class(""), false, false, false},
		{Class("exploded"), false, false, false},
	}
	for _, c := range cases {
		t.Run(string(c.class), func(t *testing.T) {
			if got := c.class.Valid(); got != c.valid {
				t.Errorf("Valid() = %v, want %v", got, c.valid)
			}
			if got := c.class.Counts(); got != c.counts {
				t.Errorf("Counts() = %v, want %v", got, c.counts)
			}
			if got := c.class.Recoverable(); got != c.recoverable {
				t.Errorf("Recoverable() = %v, want %v", got, c.recoverable)
			}
		})
	}
}

// An adapter that forgets to set a class must not be read as success.
func TestZeroClassIsNotOK(t *testing.T) {
	var r Result
	if r.Class.Valid() {
		t.Fatal("the zero Class must not validate")
	}
	if r.Class == ClassOK {
		t.Fatal("the zero Class must not equal ClassOK")
	}
}

func TestPermissionsValid(t *testing.T) {
	for _, p := range AllPermissions() {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []Permissions{"", "yolo", "Auto"} {
		if p.Valid() {
			t.Errorf("%q should not be valid", p)
		}
	}
}

func TestCapabilitiesSupports(t *testing.T) {
	caps := Capabilities{Permissions: []Permissions{PermScoped, PermAuto}}
	if !caps.Supports(PermScoped) || !caps.Supports(PermAuto) {
		t.Fatal("declared permissions should be supported")
	}
	if caps.Supports(PermBypass) {
		t.Fatal("an undeclared permission must not be supported")
	}
	if (Capabilities{}).Supports(PermAuto) {
		t.Fatal("a backend declaring nothing supports nothing")
	}
}

func TestUsageAddAndIsZero(t *testing.T) {
	var acc Usage
	if !acc.IsZero() {
		t.Fatal("a backend that reports no usage should leave a zero Usage")
	}
	acc = acc.Add(Usage{CostUSD: 0.25, InputTokens: 10, CacheReadTokens: 5})
	acc = acc.Add(Usage{CostUSD: 0.75, OutputTokens: 3, CacheCreationTokens: 7})
	want := Usage{CostUSD: 1, InputTokens: 10, OutputTokens: 3, CacheReadTokens: 5, CacheCreationTokens: 7}
	if acc != want {
		t.Fatalf("got %+v, want %+v", acc, want)
	}
	if acc.IsZero() {
		t.Fatal("an accumulated usage is not zero")
	}
}

func TestSpecRequest(t *testing.T) {
	spec := Spec{
		Provider:       "fake",
		Model:          "sonnet",
		Permissions:    PermScoped,
		AllowedTools:   []string{"Read", "Grep"},
		DeniedTools:    []string{"Bash(bd close:*)"},
		ExtraArgs:      []string{"--flag"},
		Timeout:        30 * time.Second,
		Resume:         false,
		Sandbox:        "workspace-write",
		ApprovalPolicy: "never",
		Shell:          true,
	}
	req := spec.Request(RoleReviewer)
	if req.Role != RoleReviewer || req.Model != "sonnet" || req.Permissions != PermScoped {
		t.Fatalf("spec not carried into the request: %+v", req)
	}
	if req.Timeout != 30*time.Second {
		t.Fatalf("timeout = %v", req.Timeout)
	}
	if spec.Sandbox != "workspace-write" || spec.ApprovalPolicy != "never" || !spec.Shell {
		t.Fatalf("Codex settings were not retained on Spec: %+v", spec)
	}
	if len(req.AllowedTools) != 2 || len(req.DeniedTools) != 1 || len(req.ExtraArgs) != 1 {
		t.Fatalf("tool and arg lists not carried: %+v", req)
	}

	// The request must not alias the spec's slices: one mutated request would
	// otherwise rewrite the role's configuration for every later round.
	req.AllowedTools[0] = "Bash"
	if spec.AllowedTools[0] != "Read" {
		t.Fatal("Request aliases the spec's AllowedTools")
	}
	req.DeniedTools[0] = "Read"
	if spec.DeniedTools[0] != "Bash(bd close:*)" {
		t.Fatal("Request aliases the spec's DeniedTools: a run could drop its own deny rules")
	}
}

func TestSinkFuncAndDiscard(t *testing.T) {
	var got []Event
	sink := SinkFunc(func(e Event) { got = append(got, e) })
	sink.Emit(Event{Kind: EventText, Text: "hi"})
	Emit(sink, Event{Kind: EventDone})
	if len(got) != 2 || got[0].Text != "hi" || got[1].Kind != EventDone {
		t.Fatalf("events not delivered: %+v", got)
	}

	// Both a nil sink and Discard have to be safe: adapters emit
	// unconditionally.
	Emit(nil, Event{Kind: EventText})
	Discard.Emit(Event{Kind: EventText})
	Emit(Discard, Event{Kind: EventText})
}

func TestRegistryRoundTrip(t *testing.T) {
	resetRegistry(t)
	Register("fake", func(s Spec) (Runner, error) { return &stubRunner{name: "fake", spec: s}, nil })
	Register("other", func(s Spec) (Runner, error) { return &stubRunner{name: "other", spec: s}, nil })

	got := Providers()
	if len(got) != 2 || got[0] != "fake" || got[1] != "other" {
		t.Fatalf("Providers() = %v, want sorted [fake other]", got)
	}

	r, err := New(Spec{Provider: "fake", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "fake" {
		t.Fatalf("built %q", r.Name())
	}
	if r.(*stubRunner).spec.Model != "opus" {
		t.Fatal("the factory should receive the resolved spec")
	}
}

// The realistic cause of an unknown provider is a typo in .beads-auto.yaml, so
// the error has to say what is available.
func TestNewUnknownProviderListsKnown(t *testing.T) {
	resetRegistry(t)
	if _, err := New(Spec{Provider: "claude"}); err == nil {
		t.Fatal("an empty registry must not resolve a provider")
	} else if !strings.Contains(err.Error(), "no providers are registered") {
		t.Fatalf("unhelpful error: %v", err)
	}

	Register("fake", func(Spec) (Runner, error) { return &stubRunner{name: "fake"}, nil })
	_, err := New(Spec{Provider: "cluade"})
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if !strings.Contains(err.Error(), "cluade") || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("error should name the typo and the known providers: %v", err)
	}

	if _, err := New(Spec{}); err == nil {
		t.Fatal("a spec with no provider must be an error")
	}
}

func TestRegistryPropagatesFactoryError(t *testing.T) {
	resetRegistry(t)
	boom := errors.New("no credentials")
	Register("fake", func(Spec) (Runner, error) { return nil, boom })
	if _, err := New(Spec{Provider: "fake"}); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the factory's error", err)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	resetRegistry(t)
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s should panic", name)
			}
		}()
		fn()
	}
	mustPanic("empty name", func() { Register("", func(Spec) (Runner, error) { return nil, nil }) })
	mustPanic("nil factory", func() { Register("nilf", nil) })

	Register("dup", func(Spec) (Runner, error) { return &stubRunner{name: "dup"}, nil })
	mustPanic("duplicate", func() {
		Register("dup", func(Spec) (Runner, error) { return &stubRunner{name: "dup"}, nil })
	})
}

func TestBuiltinRoles(t *testing.T) {
	want := []Role{RoleWorker, RoleReviewer, RoleIntegrator}
	got := BuiltinRoles()
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
