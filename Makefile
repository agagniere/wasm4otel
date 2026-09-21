# Repo-level checks. Per-directory Makefiles (zig/, go/testdata/) build
# artefacts; this one only runs what CI would, since there is no CI yet.

GO_MODULES := go/wazero go/wazy

.PHONY: test
test: check-hosts-in-sync
	@for module in $(GO_MODULES); do \
	    echo "== $$module"; \
	    (cd $$module && go build ./... && go vet ./... && go test -race -count=1 ./...) || exit 1; \
	done

# The ABI conformance suite is deliberately duplicated: both hosts must
# run the same tests for "a plugin cannot tell which host it is under"
# to mean anything, and Go cannot share an in-package test across two
# modules. Duplication only works if it stays duplication, so the two
# copies have to be byte-identical — a test added to one host and not
# the other is exactly the drift the suite exists to catch.
.PHONY: check-hosts-in-sync
check-hosts-in-sync:
	@diff go/wazero/component_test.go go/wazy/component_test.go \
	    && echo "== conformance suite in sync" \
	    || { echo "the two hosts' conformance suites have diverged; see the diff above" >&2; exit 1; }

.PHONY: fixtures
fixtures:
	$(MAKE) -C go/testdata
