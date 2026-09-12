.PHONY: build run clean test bench fmt vet lint proto vendor-rete docs docs-serve docs-clean docs-gen

BINARY_NAME=aperture
BUILD_DIR=bin
GO=go

# Aperture is pure-Go end to end. Unlike orbit (which links pulse's h3-go geo
# grouper through CGO), Aperture consumes only pulse's pure-Go expression
# evaluator, so CGO stays hard-off. Strip symbols + trimpath for release builds.
CGO_ENABLED=0
export CGO_ENABLED
BUILD_LDFLAGS=-s -w
BUILD_FLAGS=-ldflags="$(BUILD_LDFLAGS)" -trimpath

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

build:
	$(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/aperture

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

clean:
	rm -rf $(BUILD_DIR)

# test runs the whole suite with NO external services: CI has no containers, so
# every test here passes with no database present. The SQL provider's unit tests
# use a hand-rolled database/sql driver returning canned rows.
#
# The tests that need a live Postgres are GATED and skip by default. They share
# ONE gate, so a single exported DSN runs both against a scratch database (each
# creates and drops the schemas and tables it uses):
#
#   export APERTURE_PG_INTEGRATION=1
#   export APERTURE_PG_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable'
#   $(GO) test -run TestPostgresIntegration ./seed/
#   $(GO) test -run TestPostgresLive ./storage/postgres/
#
# The second is the storage backend's conformance run: the WHOLE storagetest
# contract against a real server, once unqualified and once pinned to a
# configured schema. CI has no service containers, so it is the only evidence
# storage/postgres behaves like storage/sqlite.
#
# The gate is deliberately fail-loud: with APERTURE_PG_INTEGRATION on and no
# APERTURE_PG_DSN the tests FAIL rather than skip, so asking for the run and
# silently not getting one cannot happen. Never put a DSN in a file — pass it in
# the environment.
test:
	$(GO) test ./...

# bench runs the performance benchmark suite in ./bench (INFORMATIONAL): it
# prints ns/op, allocs/op, and the computed p99 (p99-ns) + sustained throughput
# (checks/sec) for a cached Check on a sizable seeded model, with decision audit
# both ON and OFF, plus the bounded Enumerate benchmark.
#
# The HARD NFR assertion (p99 cached Check < 1ms AND >= 10k checks/sec/instance)
# is the gated test TestCheckNFR, kept out of the default `make test` so a
# loaded CI machine never flakes the build. Run it explicitly:
#
#   APERTURE_BENCH_ASSERT=1 $(GO) test -run TestCheckNFR ./bench/
#
# See docs/benchmarks.md for the methodology and the latest committed numbers.
bench:
	$(GO) test -run '^$$' -bench=. -benchmem ./bench/

fmt:
	$(GO) fmt ./...

# proto regenerates the Twirp service + protobuf messages from the .proto. The
# generated *.pb.go / *.twirp.go are COMMITTED (CI does not regenerate); this
# target mirrors orbit's. Requires protoc + protoc-gen-go + protoc-gen-twirp on
# PATH (paths=source_relative keeps the output beside the .proto).
proto:
	protoc -I=./internal/wire/rpc \
	  --go_out=./internal/wire/rpc --go_opt=paths=source_relative \
	  --twirp_out=./internal/wire/rpc --twirp_opt=paths=source_relative \
	  ./internal/wire/rpc/service.proto

vet:
	$(GO) vet ./...

# vendor-rete regenerates the committed Rete.js bundle at
# internal/server/static/vendor/rete/rete.min.js. This is the ONLY target that
# invokes node, and it is a MANUAL, OCCASIONAL step (version bumps / plugin
# changes) — it is deliberately NOT a dependency of build/test/CI. The normal
# build ships the committed blob and never runs node. All npm work happens in a
# throwaway temp dir (no node_modules/package-lock in the repo). See
# build/rete/build.sh + internal/server/static/vendor/rete/README.md.
vendor-rete:
	build/rete/build.sh

# lint runs go vet plus a static analyser when one is available. In a clean
# environment without staticcheck / golangci-lint on PATH the target degrades to
# a notice so `make lint` never hard-fails; CI installs staticcheck explicitly.
#
# "Available" has to mean USABLE, not merely present. The presence test alone
# left one gap wide open: a golangci-lint on PATH that was built with an older
# Go than this module targets refuses to start at all, so the target hard-failed
# in exactly the environment it was written to degrade in — and the failure
# looks like a lint error, which is how it earns a shrug and a "pre-existing"
# label instead of a fix.
#
# golangci-lint separates the two cases by exit code: 1 means it ran and found
# issues (a real failure, propagated), 3 means it could not run at all. Only 3
# degrades, and it degrades LOUDLY — the point of the notice is that nobody
# should be able to read this target's output and think static analysis ran.
lint: vet
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	elif command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; status=$$?; \
		if [ $$status -eq 3 ]; then \
			echo ""; \
			echo "lint: golangci-lint could not run (exit 3) — see its error above."; \
			echo "lint: the usual cause is a golangci-lint built with an older Go than this"; \
			echo "lint: module targets; it refuses to start rather than reporting findings."; \
			echo "lint: *** go vet passed, but STATIC ANALYSIS DID NOT RUN. ***"; \
			echo "lint: fix it with either of:"; \
			echo "lint:   go install honnef.co/go/tools/cmd/staticcheck@latest   (what CI uses)"; \
			echo "lint:   upgrade golangci-lint to a build matching this module's Go version"; \
		elif [ $$status -ne 0 ]; then \
			exit $$status; \
		fi; \
	else \
		echo "lint: no staticcheck/golangci-lint on PATH; ran go vet only (skipping static analysis)"; \
	fi

# docs builds the mdBook documentation site from docs/src into docs/book
# (gitignored output). Mermaid renders client-side via the vendored additional-js
# files (docs/mermaid.min.js + mermaid-init.js) — there are no preprocessor
# plugins. Requires mdbook on PATH.
docs:
	mdbook build docs

# docs-serve serves the book locally with live reload and opens a browser.
docs-serve:
	mdbook serve docs --open

# docs-clean removes the built book output.
docs-clean:
	rm -rf docs/book

# docs-gen regenerates the committed generated reference pages under docs/src
# from the Go source (on demand — there is no CI drift gate). Today it emits the
# error-code table from errors.Registry; later stories extend it with more
# generated pages. The output is committed; run this after changing a generated
# source (e.g. the error Registry in errors/codes.go).
docs-gen:
	$(GO) run ./internal/docsgen/errcodes -o docs/src/reference/error-codes.md
	$(GO) run ./internal/docsgen/cliref -o docs/src/reference/cli.md

.DEFAULT_GOAL := build
