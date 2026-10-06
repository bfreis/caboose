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
EMBEDDED := Dockerfile layer.Dockerfile layer-user.sh entrypoint.sh tmux.conf shellrc.bash sandbox/CLAUDE.md imagecheck.sh
GO_LDFLAGS ?=

# caboose-agent, the sandbox's end of the link, for each architecture the
# image can be: linux binaries, embedded in the launcher (embed.go) and
# COPYed into the layer. Built wherever make runs, the sandbox included --
# they are linux binaries everywhere, and gitignored.
AGENT_ARCHS := amd64 arm64
AGENT_BINS  := $(foreach a,$(AGENT_ARCHS),agent-bin/caboose-agent-linux-$(a))
# Every package of this module the agent imports, as go list says, so a new
# dependency cannot be missed (internal/linkdebug once was), and go.sum.
AGENT_SRC   := go.mod $(wildcard go.sum) $(shell go list -deps -f '{{if not .Standard}}{{$$d := .Dir}}{{range .GoFiles}}{{$$d}}/{{.}} {{end}}{{end}}' ./cmd/caboose-agent 2>/dev/null)

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
#
# -buildvcs=false: the agent's bytes are its source's alone. Go would stamp
# the commit, its date and whether the tree was dirty into its buildinfo,
# so a checkout's agent never matched a release's, and the image's context
# hash, which covers it, made every switch between the two a rebuild.
agent-bin/caboose-agent-linux-%: $(AGENT_SRC)
	@CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -buildvcs=false -ldflags '-s -w' -o $@ ./cmd/caboose-agent \
	  && echo "  built $@"

## agent: build caboose-agent for each architecture (the launcher embeds them)
agent: $(AGENT_BINS)

$(LAUNCHER): $(GO_SRC) $(EMBEDDED) $(AGENT_BINS) $(PLATFORM_STAMP) $(VERSION_STAMP)
	@{ read -r v; read -r c; read -r d; } < $(VERSION_STAMP); \
	  CGO_ENABLED=0 go build -o $@ -ldflags \
	    "-X $(VERSION_PKG).Version=$$v -X $(VERSION_PKG).Commit=$$c -X $(VERSION_PKG).Date=$$d "'$(GO_LDFLAGS)' \
	    ./cmd/caboose \
	  && echo "  built $@ $$v for $$(cat $(PLATFORM_STAMP))"

# caboose-vmm, the vm isolation's VM runner: macOS only, and signed with
# the virtualization entitlement, without which Virtualization.framework
# refuses it (goreleaser signs the released one, through macsign.sh). It
# sits next to ./caboose, gitignored, as a release installs it next to
# caboose. `make launcher` on a Mac builds it too.
#
# Signed ad-hoc by codesign, unless DEVID_P12 names a Developer ID
# Application .p12 and DEVID_P12_PASSWORD_FILE a file holding its
# password: then macsign.sh signs it with them (rcodesign, which must be
# installed, reads the .p12 itself, with no keychain), with the hardened
# runtime, and notarizes it too when APPSTORE_API_KEY_JSON names an App
# Store Connect API key's JSON (rcodesign encode-app-store-connect-api-key).
DEVID_P12               ?=
DEVID_P12_PASSWORD_FILE ?=
APPSTORE_API_KEY_JSON   ?=
VMM          := caboose-vmm
ENTITLEMENTS := cmd/caboose-vmm/entitlements.plist
ifeq ($(shell uname -s),Darwin)
VMM_ON_MAC := $(VMM)
$(VMM): $(GO_SRC) $(ENTITLEMENTS) macsign.sh $(VERSION_STAMP)
	@{ read -r v; read -r c; read -r d; } < $(VERSION_STAMP); \
	  CGO_ENABLED=0 go build -o $@ -ldflags \
	    "-X $(VERSION_PKG).Version=$$v -X $(VERSION_PKG).Commit=$$c -X $(VERSION_PKG).Date=$$d" \
	    ./cmd/caboose-vmm \
	  && if [ -n "$(DEVID_P12)" ]; then \
	       MACSIGN_P12="$(DEVID_P12)" MACSIGN_P12_PASSWORD_FILE="$(DEVID_P12_PASSWORD_FILE)" \
	       MACSIGN_API_KEY="$(APPSTORE_API_KEY_JSON)" sh macsign.sh caboose-vmm $@ $(ENTITLEMENTS); \
	     else \
	       codesign -s - --force --identifier caboose-vmm --entitlements $(ENTITLEMENTS) $@; \
	     fi \
	  && echo "  built and signed $@ $$v"
else
$(VMM):
	@echo "  !!  caboose-vmm runs VMs on macOS only: build it with 'make vmm' on a Mac" >&2; exit 1
endif

## vmm: build and sign ./caboose-vmm, the vm isolation's runner (on a Mac)
vmm: $(VMM)

# The vm isolation's builder guest, a raw ext4 disk built by docker from
# vm/builder (docker on the host, or CI): vm-dist/builder-<arch>.img,
# gitignored. VM_ARCH is the guest's, which is the Mac's.
VM_ARCH ?= arm64

## vm-builder: build vm-dist/builder-ARCH.img, the vm isolation's builder guest (docker, on the host)
vm-builder:
	@if [ -n "$(IN_SANDBOX)" ]; then echo "  !!  vm-builder drives docker: run it on the host, not in the sandbox" >&2; exit 1; fi
	@command -v docker > /dev/null || { echo "  !!  vm-builder needs docker: run it on the host, or in CI" >&2; exit 1; }
	@docker buildx build --platform linux/$(VM_ARCH) -o type=local,dest=vm-dist/.builder-$(VM_ARCH) vm/builder \
	  && mv vm-dist/.builder-$(VM_ARCH)/builder.img vm-dist/builder-$(VM_ARCH).img \
	  && rmdir vm-dist/.builder-$(VM_ARCH) \
	  && echo "  built vm-dist/builder-$(VM_ARCH).img"

# The vm isolation's guest kernel: kernel.org's 6.18 LTS, unpatched, built
# by docker from vm/kernel (docker on the host, or CI; natively, or cross
# on an amd64 runner) as vm-dist/kernel-ARCH, the uncompressed Image, and
# kernel-ARCH.config, the config it resolved to. The pin is the version
# and the sha256 of its tarball, from kernel.org's signed sha256sums.asc
# (check its signature when bumping: CLAUDE.md says how and when).
LINUX_VERSION := 6.18.54
LINUX_SHA256  := 9df30b02dd8102bbd0be52556288ef6889ddbe7f1ddb96fbf847d0becf3eacac
KERNEL_SRC    := vm/kernel/Dockerfile vm/kernel/check-config $(wildcard vm/kernel/config-*)
# More flags for its docker buildx build: --no-cache, to check that a
# build from nothing gives the same Image.
VM_KERNEL_BUILD_FLAGS ?=

# The pin as a file, rewritten only when it changes, so a bump rebuilds
# the kernel (and a checkout still holding an older one replaces it).
KERNEL_PIN := vm-dist/.kernel-pin
$(KERNEL_PIN): FORCE
	@mkdir -p vm-dist
	@want="$(LINUX_VERSION) $(LINUX_SHA256)"; \
	  [ "$$(cat $@ 2>/dev/null)" = "$$want" ] || printf '%s\n' "$$want" > $@

## vm-kernel: build vm-dist/kernel-ARCH, the vm isolation's guest kernel (docker, on the host)
# Its .SOURCE is rewritten on every run, after the kernel, so the three
# files always describe one build: a checkout that held another kernel
# (Kata's, once) would otherwise keep that kernel's .SOURCE beside this one.
vm-kernel: vm-dist/kernel-$(VM_ARCH) vm-dist/kernel-$(VM_ARCH).SOURCE
	@$(call check-kernel,vm-dist)

# Fails unless the Image's own banner and its .SOURCE both name
# LINUX_VERSION, and its .config is there, in the directory $(1): run by
# vm-kernel, and again by vm-assets just before it packs them (and on what
# it unpacks, when they were built elsewhere), so no release ships a kernel
# beside another's source.
LINUX_VERSION_RE := $(subst .,\.,$(LINUX_VERSION))
define check-kernel
k=$(1)/kernel-$(VM_ARCH); \
  LC_ALL=C grep -aqE 'Linux version $(LINUX_VERSION_RE)([^0-9.]|$$)' "$$k" \
  || { echo "  !!  $$k is not Linux $(LINUX_VERSION): delete it and run make vm-kernel" >&2; exit 1; }; \
  [ -s "$$k.config" ] \
  || { echo "  !!  $$k.config is missing: delete $$k and run make vm-kernel" >&2; exit 1; }; \
  [ "$$(head -n 1 "$$k.SOURCE" 2>/dev/null)" = "Linux $(LINUX_VERSION), unmodified, from" ] \
  || { echo "  !!  $$k.SOURCE does not name Linux $(LINUX_VERSION): run make vm-kernel" >&2; exit 1; }
endef

vm-dist/kernel-$(VM_ARCH): $(KERNEL_SRC) $(KERNEL_PIN)
	@if [ -n "$(IN_SANDBOX)" ]; then echo "  !!  vm-kernel drives docker: run it on the host, not in the sandbox" >&2; exit 1; fi
	@command -v docker > /dev/null || { echo "  !!  vm-kernel needs docker: run it on the host, or in CI" >&2; exit 1; }
	@t=vm-dist/.kernel-$(VM_ARCH); rm -rf "$$t"; \
	  docker buildx build --platform linux/$(VM_ARCH) $(VM_KERNEL_BUILD_FLAGS) \
	    --build-arg LINUX_VERSION=$(LINUX_VERSION) --build-arg LINUX_SHA256=$(LINUX_SHA256) \
	    -o type=local,dest="$$t" vm/kernel \
	  && mv "$$t/kernel.config" $@.config && mv "$$t/kernel" $@ && rmdir "$$t" \
	  && echo "  built $@ (Linux $(LINUX_VERSION))"

## vm-kernel-smoke: boot vm-dist/kernel-ARCH under QEMU with a test init, checking what caboose needs of it (docker)
# The init (vm/kernel/smoke/init) checks the mounts, devices, namespaces
# and decisions the sandbox relies on, in an initramfs written as the
# launcher writes its own; QEMU runs in a container, emulating, so it
# needs no KVM and runs on any host with docker, or in CI.
vm-kernel-smoke: vm-dist/kernel-$(VM_ARCH)
	@if [ -n "$(IN_SANDBOX)" ]; then echo "  !!  vm-kernel-smoke drives docker: run it on the host, not in the sandbox" >&2; exit 1; fi
	@[ "$(VM_ARCH)" = arm64 ] || { echo "  !!  vm-kernel-smoke boots arm64 guests only" >&2; exit 1; }
	@set -e; t=vm-dist/.smoke-$(VM_ARCH); rm -rf "$$t"; mkdir -p "$$t"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$(VM_ARCH) go build -trimpath -o "$$t/init" ./vm/kernel/smoke/init; \
	  go run ./vm/kernel/smoke/initramfs "$$t/init" > "$$t/initramfs.cpio"; \
	  cp vm-dist/kernel-$(VM_ARCH) "$$t/kernel"; \
	  img=$$(docker build -q vm/kernel/smoke); \
	  docker run --rm -v "$$PWD/$$t:/smoke:ro" "$$img" | tee "$$t/console.log"; \
	  if grep -q '^SMOKE PASS' "$$t/console.log"; then \
	    echo "  ok  vm-dist/kernel-$(VM_ARCH) booted and passed (console: $$t/console.log)"; \
	  else \
	    echo "  !!  vm-dist/kernel-$(VM_ARCH) failed its smoke test: see the FAIL lines above, or $$t/console.log" >&2; exit 1; \
	  fi

## vm-kernel-pin: print the guest kernel's pin, LINUX_VERSION and LINUX_SHA256
vm-kernel-pin:
	@echo "$(LINUX_VERSION) $(LINUX_SHA256)"

# Where the kernel's source is, shipped beside it: the tarball it was
# built from, kept with every config a release built from it in a release
# of its own (kernel-VERSION, made by release.yml in the repository the
# release is), and the recipe at the commit that built it.
RELEASE_OWNER ?= bfreis
RELEASE_REPO  ?= caboose
KERNEL_SOURCE_URL := https://github.com/$(RELEASE_OWNER)/$(RELEASE_REPO)/releases/tag/kernel-$(LINUX_VERSION)
vm-dist/kernel-$(VM_ARCH).SOURCE: vm-dist/kernel-$(VM_ARCH) $(KERNEL_PIN) FORCE
	@commit=$$(git rev-parse HEAD); \
	  printf '%s\n' \
	  "Linux $(LINUX_VERSION), unmodified, from" \
	  "https://cdn.kernel.org/pub/linux/kernel/v$(firstword $(subst ., ,$(LINUX_VERSION))).x/linux-$(LINUX_VERSION).tar.xz" \
	  "(sha256 $(LINUX_SHA256))." \
	  "" \
	  "The exact tarball, and this config as kernel-$(VM_ARCH)-$(VM_VERSION).config:" \
	  "$(KERNEL_SOURCE_URL)" \
	  "" \
	  "Its config is kernel-$(VM_ARCH).config beside this file, the full resolved" \
	  "config. It was built by vm/kernel/ at caboose $(VM_VERSION), commit $$commit:" \
	  "https://github.com/$(RELEASE_OWNER)/$(RELEASE_REPO)/tree/$$commit/vm/kernel" > $@

# vm's files as one release asset for VM_ARCH, which goreleaser adds to a
# release (and to its checksums.txt), and a release build fetches:
# vm-release/caboose-vm_VERSION_ARCH.tar.gz. Needs docker, for the kernel and the builder.
VM_VERSION ?= dev

VM_ASSET := vm-release/caboose-vm_$(VM_VERSION)_$(VM_ARCH).tar.gz
VM_ASSET_FILES := kernel-$(VM_ARCH) kernel-$(VM_ARCH).config kernel-$(VM_ARCH).SOURCE builder-$(VM_ARCH).img

# VM_ASSETS_PREBUILT=1: the asset was built elsewhere and is already in
# vm-release/ (release.yml builds it on an arm64 runner, where the kernel
# is the one a Mac's rebuild gives, and hands it to goreleaser's job).
# Nothing is built: the asset is unpacked aside and checked as a build's
# files would be, and that it holds those files and was made for
# VM_VERSION, or goreleaser stops before it publishes.
VM_ASSETS_PREBUILT ?=

## vm-assets: pack vm's kernel and builder as vm-release/caboose-vm_VERSION_ARCH.tar.gz (docker)
ifeq ($(VM_ASSETS_PREBUILT),1)
vm-assets:
	@[ -s $(VM_ASSET) ] || { echo "  !!  VM_ASSETS_PREBUILT=1, but there is no $(VM_ASSET): build it with make vm-assets VM_VERSION=$(VM_VERSION) (release.yml's vm-assets job), or leave VM_ASSETS_PREBUILT unset to build it here" >&2; exit 1; }
	@set -e; t=vm-dist/.prebuilt-$(VM_ARCH); rm -rf "$$t"; mkdir -p "$$t"; \
	  have=$$(tar -tzf $(VM_ASSET) | LC_ALL=C sort | tr '\n' ' '); \
	  want=$$(printf '%s\n' $(VM_ASSET_FILES) | LC_ALL=C sort | tr '\n' ' '); \
	  [ "$$have" = "$$want" ] || { echo "  !!  $(VM_ASSET) holds $$have, not $$want" >&2; exit 1; }; \
	  tar -xzf $(VM_ASSET) -C "$$t"; \
	  grep -qF "at caboose $(VM_VERSION), commit" "$$t/kernel-$(VM_ARCH).SOURCE" \
	  || { echo "  !!  $(VM_ASSET)'s kernel-$(VM_ARCH).SOURCE was not made for caboose $(VM_VERSION)" >&2; exit 1; }; \
	  [ -s "$$t/builder-$(VM_ARCH).img" ] || { echo "  !!  $(VM_ASSET)'s builder-$(VM_ARCH).img is empty" >&2; exit 1; }
	@$(call check-kernel,vm-dist/.prebuilt-$(VM_ARCH))
	@rm -rf vm-dist/.prebuilt-$(VM_ARCH)
	@ls -l $(VM_ASSET)
else
vm-assets: vm-kernel vm-builder
	@$(call check-kernel,vm-dist)
	@mkdir -p vm-release
	@tar -czf $(VM_ASSET) -C vm-dist $(VM_ASSET_FILES) \
	  && ls -l $(VM_ASSET)
endif

## launcher: build ./caboose from the Go sources (only when something changed)
launcher: $(LAUNCHER) $(VMM_ON_MAC)

# The environments these targets drive. The checkout's launcher stays out of
# the environment you work in (the default one, on a release, which moves
# only when it updates itself): build, restart and the rest act on DEV_ENV,
# test on TEST_ENV. DEV_ENV=default aims them at your own, when you mean it.
DEV_ENV  ?= dev
TEST_ENV ?= test

# make-env NAME: creates environment NAME when it does not exist yet (the
# launcher refuses one that does not, in case of a typo), saying so; it then
# runs on defaults until 'caboose -e NAME setup' asks for its settings.
define make-env
d="$${CABOOSE_HOME:-$$HOME/.caboose}/envs/$(1)"; \
if [ "$(1)" != default ] && [ ! -d "$$d" ]; then \
  mkdir -p "$$d" && echo "  created environment '$(1)' ($$d), on defaults: '$(CC) -e $(1) setup' asks for its settings"; \
fi
endef

## build: rebuild DEV_ENV's image (default dev; does not touch your installed Claude Code)
build: $(CC)
	@$(call make-env,$(DEV_ENV)); CABOOSE_ENV=$(DEV_ENV) $(CC) build

## restart: recreate DEV_ENV's container to pick up a rebuilt image (asks; FORCE=1 skips)
restart: $(CC)
	@$(call make-env,$(DEV_ENV)); CABOOSE_ENV=$(DEV_ENV) $(CC) restart

## stop: stop DEV_ENV's container (asks first; FORCE=1 skips)
stop: $(CC)
	@CABOOSE_ENV=$(DEV_ENV) $(CC) stop

## status: DEV_ENV's container, version, live sessions and disk use
status: $(CC)
	@CABOOSE_ENV=$(DEV_ENV) $(CC) status

## prune: delete DEV_ENV's old installed Claude Code versions now
prune: $(CC)
	@CABOOSE_ENV=$(DEV_ENV) $(CC) prune

## logs: DEV_ENV's supervisor log (bootstrap, pruning, stale-state clearing)
logs: $(CC)
	@CABOOSE_ENV=$(DEV_ENV) $(CC) logs --tail 50

## shell: bash prompt inside DEV_ENV's container
shell: $(CC)
	@$(call make-env,$(DEV_ENV)); CABOOSE_ENV=$(DEV_ENV) $(CC) shell

STATICCHECK_VERSION := v0.8.1

# What CI installs, so the version is said once, here.
staticcheck-version:
	@echo $(STATICCHECK_VERSION)

## lint: bash -n / sh -n + shellcheck over the shell scripts, gofmt + go vet + staticcheck
# CI runs this target as it is. A missing shellcheck or staticcheck fails it
# rather than skipping, since a skipped check reads as a passed one:
# STATICCHECK_VERSION is the one CI installs.
# imagecheck.sh, layer-user.sh and install.sh get sh -n, not bash -n: they
# run on images with no bash (or before its absence is known), or on a
# machine nothing is known about yet, and shellcheck reads their #!/bin/sh
# as a request to hold them to POSIX sh.
lint:
	@set -e; for f in entrypoint.sh shellrc.bash tests/run.sh tests/byo/run.sh; do \
	    bash -n "$$f" && echo "  ok  $$f"; \
	  done; \
	  for f in imagecheck.sh layer-user.sh install.sh macsign.sh vm/builder/caboose-builder vm/kernel/check-config vm/kernel/smoke/run; do \
	    sh -n "$$f" && echo "  ok  $$f"; \
	  done; \
	  command -v shellcheck >/dev/null 2>&1 || { \
	    echo "  !!  shellcheck is not installed: https://github.com/koalaman/shellcheck#installing"; exit 1; }; \
	  shellcheck -S warning entrypoint.sh shellrc.bash tests/run.sh tests/byo/run.sh imagecheck.sh layer-user.sh install.sh macsign.sh vm/builder/caboose-builder vm/kernel/check-config vm/kernel/smoke/run \
	    && echo "  ok  shellcheck"; \
	  unformatted="$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"; \
	  if [ -n "$$unformatted" ]; then \
	    echo "  !!  gofmt: $$unformatted"; exit 1; \
	  fi; echo "  ok  gofmt"; \
	  go vet ./... && echo "  ok  go vet"; \
	  for os in linux darwin; do \
	    [ "$$os" = "$$(go env GOOS)" ] || { GOOS=$$os go vet ./... && echo "  ok  go vet ($$os)"; }; \
	  done; \
	  command -v staticcheck >/dev/null 2>&1 || { \
	    echo "  !!  staticcheck is not installed: go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)"; exit 1; }; \
	  for os in linux darwin; do \
	    GOOS=$$os staticcheck ./... && echo "  ok  staticcheck ($$os)"; \
	  done

## go-test: run the Go unit tests (no docker, safe anywhere)
# The agent first: the image's context hashes cover its binaries.
go-test: $(AGENT_BINS)
	@go test ./...

## test: build TEST_ENV's image (default test), then run the integration suite in it
# The suite recreates that environment's container, never yours. caboose-vmm
# too, on a Mac: the vm group runs it beside ./caboose, and the two must be
# the same version.
test: $(CC) $(VMM_ON_MAC)
	@$(call make-env,$(TEST_ENV)); CABOOSE_ENV=$(TEST_ENV) $(CC) build
	@CABOOSE_ENV=$(TEST_ENV) CABOOSE_BIN=$(CC) ./tests/run.sh

## test-vm: run only the integration suite's isolation vm group (on a Mac)
# Not `build`: the group uses an environment of its own, a throwaway, whose
# image it builds in the builder guest. `launcher` also builds and signs
# caboose-vmm beside ./caboose, on a Mac.
test-vm: launcher
	@CABOOSE_BIN=$(CC) ONLY=vm ./tests/run.sh

## test-byo: run the bring-your-own-image suite (slow, installs Claude Code twice)
# Not part of `test`: it builds two test bases and installs Claude Code twice
# (glibc and musl, ~224MB each). It needs only the launcher, not `build`: it
# builds its own images, under names of its own, with a throwaway data dir,
# so it leaves the real container, image and data dir alone.
test-byo: $(CC)
	@CABOOSE_BIN=$(CC) ./tests/byo/run.sh

.PHONY: FORCE help staticcheck-version agent vmm vm-builder vm-kernel vm-kernel-pin vm-kernel-smoke vm-assets launcher build restart stop status prune logs shell lint go-test test test-vm test-byo
FORCE:
