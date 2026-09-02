GOTOOLCHAIN ?= go1.27.0
BASE ?=

.PHONY: test test-race test-properties mutation-changed mutation-full

test:
	GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./...

test-race:
	GOTOOLCHAIN=$(GOTOOLCHAIN) go test -race ./...

test-properties:
	RAPID_CHECKS=1000 GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./... -run 'Property|Model'

mutation-changed:
	@test -n "$(BASE)" || { echo "BASE=<commit> is required"; exit 2; }
	GOTOOLCHAIN=$(GOTOOLCHAIN) ./scripts/mutation-run changed "$(BASE)"

mutation-full:
	GOTOOLCHAIN=$(GOTOOLCHAIN) ./scripts/mutation-run full
