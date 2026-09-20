GOTOOLCHAIN ?= go1.27.0
BASE ?=

.PHONY: mutation-changed mutation-full

mutation-changed:
	@test -n "$(BASE)" || { echo "BASE=<verified-base-commit> is required" >&2; exit 2; }
	GOTOOLCHAIN=$(GOTOOLCHAIN) sh ./scripts/mutation-run changed "$(BASE)"

mutation-full:
	GOTOOLCHAIN=$(GOTOOLCHAIN) sh ./scripts/mutation-run full
