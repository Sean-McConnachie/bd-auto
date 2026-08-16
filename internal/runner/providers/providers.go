// Package providers registers every shipped runner adapter.
//
// Adapters register themselves from an init, so a provider that nothing imports
// does not exist: runner.New would report `unknown provider "claude"` with an
// empty list of known ones, which is a confusing way to say "the binary was
// built without the backend". Importing this package once, from the engine, is
// what makes the registry match what bd-auto actually ships.
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
