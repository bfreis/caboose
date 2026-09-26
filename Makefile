# Maintenance tasks for the caboose sandbox.
#
# Attaching a session is deliberately NOT a target: the launcher derives the
# tmux session and the container path from $PWD, so it has to be run
# from the project you want to work on. `make attach` from here could only ever
# open a session on caboose itself. Run `caboose` in the repo instead.

SHELL := /bin/bash

# The launcher is a Go binary built at the repo root, gitignored. It lives
# there rather than in bin/ because PATH symlinks point at <checkout>/caboose,
# and the launcher looks for its checkout next to its own executable.
LAUNCHER := caboose
CC       := ./$(LAUNCHER)

# Everything the binary is built from. The image's build inputs are embedded
# (see embed.go), so editing the Dockerfiles, entrypoint.sh or tmux.conf has to
# rebuild the launcher before `make build` can see the change -- listing them
# here is what makes that automatic. Test files do not reach the binary.
GO_SRC   := go.mod $(wildcard go.sum) \
            $(shell find . -name '*.go' -not -name '*_test.go' -not -path './.*')
EMBEDDED := Dockerfile layer.Dockerfile layer-user.sh entrypoint.sh tmux.conf sandbox/CLAUDE.md imagecheck.sh
GO_LDFLAGS ?=

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' \
	  | awk -F': ' '{printf "  \033[1m%-10s\033[0m %s\n", $$1, $$2}'

# Building ./caboose inside the sandbox would overwrite the host's binary with
# a linux one. The entrypoint's path is what marks this image: it exists on no
# host. A build to any other path (make launcher LAUNCHER=/tmp/caboose) is
# harmless and allowed; `make go-test` and `make lint` (go vet) already
# compile every package without writing anything.
IN_SANDBOX := $(wildcard /usr/local/bin/caboose-entrypoint)
ifneq ($(IN_SANDBOX),)
ifeq ($(abspath $(LAUNCHER)),$(abspath caboose))
SANDBOX_GUARD = @printf '%s\n' \
  "refusing to build ./$(LAUNCHER) inside the caboose sandbox: this checkout is" \
  "shared with the host, and a linux binary here replaces the host's own." \
  "Build it on the host. To check it compiles in here: make go-test or make lint," \
  "or build elsewhere: make launcher LAUNCHER=/tmp/caboose" >&2; exit 1
endif
endif

# The checkout is shared with the sandbox at the same path, so a binary built
# in there is a linux one sitting where the host expects its own -- and being
# newer than its sources, mtimes alone would never replace it ("cannot execute
# binary file"). The stamp records the platform it was built for; its rule
# always runs but rewrites the file only when that differs, so a matching
# platform leaves make a no-op and a different one forces a rebuild.
PLATFORM_STAMP := $(LAUNCHER).platform
$(PLATFORM_STAMP): FORCE
	$(SANDBOX_GUARD)
	@want="$$(go env GOOS GOARCH | paste -s -d / -)"; \
	  [ -n "$$want" ] || { echo "  !!  go env failed; is Go installed?" >&2; exit 1; }; \
	  [ "$$(cat $@ 2>/dev/null)" = "$$want" ] || printf '%s\n' "$$want" > $@

# The version stamped into the launcher (internal/version), recorded the same
# way: `git describe`, the commit and its date, rewritten only when they
# change. A new commit or tag rebuilds once and repeated runs stay no-ops.
# The date is the commit's, not the build's -- a clock here would change on
# every run. --dirty flips on with the first uncommitted edit anywhere in the
# tree, which rebuilds once; a Go edit rebuilds anyway, and further edits
# leave the string as it is. Without git (a tarball, say), or in a repo with
# no commits yet, it is "dev" with no commit or date: --verify -q makes
# rev-parse print nothing there, where plain `rev-parse HEAD` echoes "HEAD".
# want goes through a command substitution, as `cat` does, so both sides
# lose their trailing newlines alike -- empty last lines would otherwise
# never compare equal, and the launcher would relink on every run.
VERSION_STAMP := $(LAUNCHER).version
VERSION_PKG   := github.com/bfreis/caboose/internal/version
$(VERSION_STAMP): FORCE
	$(SANDBOX_GUARD)
	@want="$$(printf '%s\n' \
	    "$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
	    "$$(git rev-parse --verify -q HEAD 2>/dev/null)" \
	    "$$(git log -1 --format=%cI 2>/dev/null)")"; \
	  [ "$$(cat $@ 2>/dev/null)" = "$$want" ] || printf '%s\n' "$$want" > $@

# CGO_ENABLED=0 always: the launcher needs no C, a static binary is what the
# releases ship, and a `make CC=...` override is exported into recipes,
# where cgo would read it as the C compiler. GO_LDFLAGS goes after the stamp,
# so an -X in it overrides the stamped value. The stamp may end early (no
# commit, no date): a read past its end fails and leaves the variable empty, and an empty -X is what version.resolve expects
# of an unknown commit or date.
$(LAUNCHER): $(GO_SRC) $(EMBEDDED) $(PLATFORM_STAMP) $(VERSION_STAMP)
	@{ read -r v; read -r c; read -r d; } < $(VERSION_STAMP); \
	  CGO_ENABLED=0 go build -o $@ -ldflags \
	    "-X $(VERSION_PKG).Version=$$v -X $(VERSION_PKG).Commit=$$c -X $(VERSION_PKG).Date=$$d "'$(GO_LDFLAGS)' \
	    ./cmd/caboose \
	  && echo "  built $@ $$v for $$(cat $(PLATFORM_STAMP))"

## launcher: build ./caboose from the Go sources (only when something changed)
launcher: $(LAUNCHER)

## build: rebuild the image (does not touch your installed Claude Code)
build: $(CC)
	@$(CC) build

## restart: recreate the container to pick up a rebuilt image (asks; FORCE=1 skips)
restart: $(CC)
	@$(CC) restart

## stop: stop the container (asks first; FORCE=1 skips)
stop: $(CC)
	@$(CC) stop

## status: container, version, live sessions and disk use
status: $(CC)
	@$(CC) status

## prune: delete old installed Claude Code versions now
prune: $(CC)
	@$(CC) prune

## logs: supervisor log (bootstrap, pruning, stale-state clearing)
logs: $(CC)
	@$(CC) logs --tail 50

## shell: bash prompt inside the container
shell: $(CC)
	@$(CC) shell

## lint: bash -n / sh -n + shellcheck over the shell scripts, gofmt + go vet + staticcheck
# imagecheck.sh, layer-user.sh and install.sh get sh -n, not bash -n: they
# run on images with no bash (or before its absence is known), or on a
# machine nothing is known about yet, and shellcheck reads their #!/bin/sh
# as a request to hold them to POSIX sh.
lint:
	@set -e; for f in entrypoint.sh tests/run.sh tests/byo/run.sh; do \
	    bash -n "$$f" && echo "  ok  $$f"; \
	  done; \
	  for f in imagecheck.sh layer-user.sh install.sh; do \
	    sh -n "$$f" && echo "  ok  $$f"; \
	  done; \
	  if command -v shellcheck >/dev/null 2>&1; then \
	    shellcheck -S warning entrypoint.sh tests/run.sh tests/byo/run.sh imagecheck.sh layer-user.sh install.sh \
	      && echo "  ok  shellcheck"; \
	  else \
	    echo "  --  shellcheck not installed, skipped"; \
	  fi; \
	  unformatted="$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"; \
	  if [ -n "$$unformatted" ]; then \
	    echo "  !!  gofmt: $$unformatted"; exit 1; \
	  fi; echo "  ok  gofmt"; \
	  go vet ./... && echo "  ok  go vet"; \
	  if command -v staticcheck >/dev/null 2>&1; then \
	    staticcheck ./... && echo "  ok  staticcheck"; \
	  else \
	    echo "  --  staticcheck not installed, skipped"; \
	  fi

## go-test: run the Go unit tests (no docker, safe anywhere)
go-test:
	@go test ./...

## test: build, then run the integration suite (recreates the container)
test: build
	@CABOOSE_BIN=$(CC) ./tests/run.sh

## test-byo: run the bring-your-own-image suite (slow, installs Claude Code twice)
# Not part of `test`: it builds two test bases and installs Claude Code twice
# (glibc and musl, ~224MB each). It needs only the launcher, not `build`: it
# builds its own images, under names of its own, with a throwaway data dir,
# so it leaves the real container, image and data dir alone.
test-byo: $(CC)
	@CABOOSE_BIN=$(CC) ./tests/byo/run.sh

.PHONY: FORCE help launcher build restart stop status prune logs shell lint go-test test test-byo
FORCE:
