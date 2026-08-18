// Package providers registers every shipped runner adapter.
//
// Adapters register themselves from an init, so a provider that nothing imports
// does not exist: runner.New would report `unknown provider "claude"` with an
// empty list of known ones, which is a confusing way to say "the binary was
// built without the backend". Importing this package is what makes the registry
// match what bd-auto actually ships.
//
// internal/config is one of the importers, and the load-bearing one: it checks
// a runners: entry's provider: against the registry, so the list this package
// builds is also the list a typo is reported against. Anything that can load a
// config therefore has the adapters, whether or not it imports this itself.
//
// fake is registered alongside claude on purpose. It is the only way to run a
// whole drain — smoke test, CI, a config with `provider: fake` — without
// spending anything, and that has to work in the shipped binary rather than
// only under go test.
package providers

import (
	_ "bd-auto/internal/runner/claude"
	_ "bd-auto/internal/runner/fake"
)
