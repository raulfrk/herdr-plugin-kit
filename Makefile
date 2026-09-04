GOTOOLCHAIN ?= go1.27.0
BASE ?=

.PHONY: test test-timing test-race test-properties mutation-changed mutation-full

test:
	$(MAKE) test-timing
	HERDR_PLUGIN_KIT_TIMING= GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./...

test-timing:
	GOFLAGS= HERDR_PLUGIN_KIT_TIMING=1 GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./ui/shell -run '^TestWorstSupportedResizeBurstP95$$' -count=1
	GOFLAGS= HERDR_PLUGIN_KIT_TIMING=1 GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./ui/shell -run '^TestPTYResizeOutputAndSettlementTiming$$' -count=1

test-race:
	GOTOOLCHAIN=$(GOTOOLCHAIN) go test -race ./...

test-properties:
	RAPID_CHECKS=1000 GOTOOLCHAIN=$(GOTOOLCHAIN) go test ./... -run 'Property|Model'

mutation-changed:
	@test -n "$(BASE)" || { echo "BASE=<commit> is required"; exit 2; }
	GOTOOLCHAIN=$(GOTOOLCHAIN) ./scripts/mutation-run changed "$(BASE)"

mutation-full:
	GOTOOLCHAIN=$(GOTOOLCHAIN) ./scripts/mutation-run full
