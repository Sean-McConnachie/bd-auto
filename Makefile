GO ?= go
BIN := bin/bd-auto
PKG := ./cmd/bd-auto

.PHONY: build test vet fmt check smoke launch-cost resume-vs-fresh clean install-check

# The plugin puts bin/ on PATH, so workers find bd-auto once this has run.
build:
	$(GO) build -o $(BIN) $(PKG)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w ./cmd ./internal

# The same commands the gate runs, so you can reproduce a gate failure locally.
check: build vet test

# End-to-end test against a throwaway epic. Creates and deletes its own beads
# issues and git branches, so it is kept out of `check` and out of the gate.
smoke: build
	bash scripts/smoke.sh

# What a drain costs the session that launches it — the project's headline
# claim. Spawns nothing; re-run it after touching SKILL.md or the poll view.
launch-cost:
	bash scripts/launch-cost.sh

# Re-measure what recovery costs each way. Spawns real models and spends real
# money, in its own throwaway repo; never part of check or the gate.
resume-vs-fresh: build
	bash scripts/resume-vs-fresh.sh

# Verify the plugin manifest and components load.
install-check: build
	claude plugin validate .

clean:
	rm -f $(BIN)
