package providers

import (
	"reflect"
	"testing"

	"bd-auto/internal/config"
	"bd-auto/internal/runner"
)

// The registry has to match what ships, or a repo's runners: block names a
// provider the binary cannot build.
func TestShippedProviders(t *testing.T) {
	want := []string{"claude", "fake"}
	if got := runner.Providers(); !reflect.DeepEqual(got, want) {
		t.Errorf("Providers = %v, want %v", got, want)
	}
}

// The end of the seam, from config to a live runner: the default role resolves
// to a claude runner, and the reviewer to a scoped one on the cheap model.
func TestBuildsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	for _, role := range runner.BuiltinRoles() {
		spec := cfg.Runner(string(role))
		r, err := runner.New(spec)
		if err != nil {
			t.Fatalf("%s: runner.New: %v", role, err)
		}
		if r.Name() != "claude" {
			t.Errorf("%s: provider = %q, want claude", role, r.Name())
		}
		if !r.Caps().Supports(spec.Permissions) {
			t.Errorf("%s: the backend cannot express %q", role, spec.Permissions)
		}
	}
}
