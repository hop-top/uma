.PHONY: build test lint tidy-check work-check release-check gen gen-check links check check-changed clean setup \
       promote promote-alpha promote-beta promote-rc \
       promote-release

# Library: no binary. Every directory holding a go.mod is a module (root,
# the adapter/, store/, contrib/ and testing/ submodules and the unreleased
# examples/ module); every target loops over all of them.
MODULES := $(sort $(patsubst ./%,%,$(patsubst %/go.mod,%,$(shell find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*'))))

# Gates run each module on its own go.mod, as a consumer resolves it. go.work
# is a development convenience (editors, cross-module edits); in workspace
# mode a missing require on hop.top/uma would still build.
export GOWORK := off

# Modules with sqlc-generated queries: every directory holding a sqlc.yaml.
# The generated code is committed; sqlc comes from mise (.config/mise.toml).
SQLC_MODULES := $(sort $(patsubst ./%,%,$(patsubst %/sqlc.yaml,%,$(shell find . -name sqlc.yaml -not -path './.git/*' -not -path '*/testdata/*'))))

# Extra `go test` flags, e.g. TESTFLAGS="-race -coverprofile=coverage.out".
TESTFLAGS ?=

check: work-check tidy-check release-check gen-check lint test links

# Scoped gate: only what the diff against origin/next can affect; see
# scripts/check-changed.sh. Falls back to `make check` when gate inputs change.
check-changed:
	@scripts/check-changed.sh

build:
	@for m in $(MODULES); do (cd $$m && go build -buildvcs=false ./...) || exit 1; done

test:
	@for m in $(MODULES); do (cd $$m && go test -buildvcs=false $(TESTFLAGS) ./...) || exit 1; done

lint:
	@for m in $(MODULES); do (cd $$m && go vet ./... && golangci-lint run ./... && golangci-lint fmt --diff ./...) || exit 1; done

# go.mod/go.sum are tidy in every module.
tidy-check:
	@for m in $(MODULES); do (cd $$m && go mod tidy -diff) || { echo "go mod tidy needed in $$m"; exit 1; }; done

# go.work lists exactly the modules on disk and loads: its go directive is
# at least every module's (`go work use` raises it).
work-check:
	@GOWORK="$(CURDIR)/go.work" go list -m >/dev/null || { echo "go.work does not load; run go work use"; exit 1; }
	@want="$$(printf '%s\n' $(MODULES))"; \
	have="$$(go work edit -json go.work | sed -n 's|.*"DiskPath": "\(.*\)".*|\1|p' | sed 's|^\./||' | LC_ALL=C sort)"; \
	if [ "$$want" != "$$have" ]; then \
		echo "go.work out of sync with modules on disk"; \
		echo "on disk:"; echo "$$want"; echo "go.work:"; echo "$$have"; exit 1; \
	fi

# release-please wiring: every module is a package or an explicit exclude, and
# every in-repo require carries the marker at the linked version; see
# scripts/release-check.sh.
release-check:
	@scripts/release-check.sh

# Regenerate query code from migrations/ and queries/.
gen:
	@for m in $(SQLC_MODULES); do (cd $$m && sqlc generate) || exit 1; done

# Committed generated code matches what sqlc produces now.
gen-check:
	@for m in $(SQLC_MODULES); do (cd $$m && sqlc diff) || { echo "generated code stale in $$m; run make gen"; exit 1; }; done

links:
	@if command -v lychee >/dev/null 2>&1; then \
		lychee --no-progress .; \
	else \
		echo "lychee not installed; skipping link check"; \
	fi

clean:
	@for m in $(MODULES); do rm -f $$m/coverage.out; done

setup:
	@for m in $(MODULES); do (cd $$m && go mod download) || exit 1; done
	@command -v lychee >/dev/null 2>&1 || cargo install lychee

promote:
	@scripts/promote-release.sh

promote-alpha promote-beta promote-rc promote-release:
	@scripts/promote-release.sh $(subst promote-,,$@)
