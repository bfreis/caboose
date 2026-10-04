# Working on caboose

Building from a checkout needs Go (the version in `go.mod`) as well as
Docker. `make help` lists the targets; the ones that matter:

| | |
|---|---|
| `make launcher` | build `./caboose` from the Go sources, only when something changed (and `./caboose-vmm` too, on a Mac) |
| `make vm-kernel` | build `vm-dist/kernel-arm64` (and its `.config` and `.SOURCE`, checked to name the pinned version), the vm isolation's guest kernel, from `vm/kernel` and the kernel.org release the Makefile pins; with docker, on the host, never in the sandbox |
| `make vm-assets` | pack the kernel and builder as `vm-release/caboose-vm_VERSION_arm64.tar.gz`, the release asset (goreleaser runs it) |
| `make vm-builder` | build `vm-dist/builder-arm64.img`, the vm isolation's builder guest, with docker; on the host, never in the sandbox |
| `make vmm` | build `./caboose-vmm`, the vm isolation's VM runner, and sign it with the virtualization entitlement: ad-hoc, or with a Developer ID (below); a Mac only |
| `make build` | the launcher, then the image (`caboose build`) |
| `make restart` / `make stop` / `make status` | the launcher's commands, launcher rebuilt first |
| `make lint` | `bash -n` + shellcheck, gofmt + go vet (+ staticcheck when installed) |
| `make go-test` | the launcher's Go unit tests; touches no container |
| `make test` | build, then run the integration suite (`tests/run.sh`) |
| `make test-vm` | only the integration suite's isolation vm group (`ONLY=vm tests/run.sh`): a throwaway environment, its image built in the builder guest; a Mac only, skipped elsewhere |
| `make test-byo` | the bring-your-own-image suite (`tests/byo/`): slow, it installs Claude Code twice |

`./caboose` is gitignored, and symlinked onto `PATH` it finds its checkout
through the link. Every make target that runs it rebuilds it first when its
sources changed, so after pulling any `make` target brings the symlinked
binary up to date. It also rebuilds when the binary is for another platform
than the host's (`caboose.platform` records which), and stamps the version
from `git describe` through `caboose.version`, rewritten only when it
changes. It refuses to build `./caboose` inside the sandbox, which shares
this checkout with the host: a linux binary there would replace the host's.

The integration suite drives the real launcher against the real container
and data dir: it recreates the container, so it kills running sessions and
refuses to start while any exist unless `FORCE=1`. It cannot run from inside
the sandbox. `make test-byo` is kept out of `make test` because it is slow:
it builds a Debian and an Alpine test image, runs a session on each against
one throwaway data dir, and so installs Claude Code once per platform. It
runs on the host too. The suite's last group, isolation vm, never touches
the real environment either: it builds, boots and tears down a VM of its own
in a throwaway data dir, every step bounded in time, and skips, saying why,
anywhere but a Mac with `caboose-vmm`, the kernel and the builder disk;
`make test-vm` runs it alone. CI runs gofmt, go vet, go test and shellcheck.

**Changes to the image need a rebuild, on the host.** `Dockerfile`,
`layer.Dockerfile`, `layer-user.sh`, `entrypoint.sh`, `tmux.conf` and
`shellrc.bash` are baked into the image, which is built from the copies embedded in the
launcher, so an edit needs `make build && make
restart` (make rebuilds the launcher for you). A launcher change takes effect
on the next run, but anything it decides at container creation — mounts,
environment — needs `make restart` too. `sandbox/CLAUDE.md` is the
exception: a launcher run from the checkout reads it from there on every
launch, so an edit reaches the next session with no rebuild. Its
placeholders are the launcher's to fill in, though: after pulling, rebuild
`./caboose` (`make launcher`, on the host), or a placeholder newer than the
binary can go unexpanded.

The image is not built from the checkout. `caboose build` writes the
files embedded in the binary into empty temp dirs and builds there — the
`Dockerfile` alone for the base, then `layer.Dockerfile`, `layer-user.sh`,
`entrypoint.sh`, `tmux.conf` and `shellrc.bash` for the layer — so nothing else in the tree
— `.git`, `.jj`, whatever is lying around — can reach the Docker daemon.
That is an allowlist by construction: a file `layer.Dockerfile` starts to
`COPY` has to be added to `embed.go` and the layer's context too, or the
build fails (the base `Dockerfile` COPYs nothing, and a test keeps it that
way). The image is labelled with the launcher version, hashes of those
contexts, the base's name and ID, and the platform, which is what
`caboose version` compares and what picks the `local/<platform>` dir to mount.
`imagecheck.sh`, the probe `caboose check-image` runs, is embedded too but is
part of neither image: an edit to it needs only the launcher rebuilt.

Releases are cut by pushing a `v*` tag: goreleaser (`.goreleaser.yaml`, run
by `.github/workflows/release.yml`) builds darwin/linux × amd64/arm64
binaries and publishes a GitHub release. Only the launcher is released; the
image is always built locally.

### Signing the macOS binaries

The darwin archives hold two binaries, `caboose` and `caboose-vmm`, which
`macsign.sh` signs with `rcodesign` in goreleaser's post-build hooks, before
anything is archived, so the archives and `checksums.txt` hold the signed
binaries. How depends on five GitHub Actions secrets of the repository the
release runs in:

| secret | what it is |
|---|---|
| `MACOS_P12_BASE64` | the Developer ID Application certificate and its private key, exported from Keychain Access as a `.p12`, base64-encoded (`base64 -i cert.p12`) |
| `MACOS_P12_PASSWORD` | the password the `.p12` was exported with |
| `APPSTORE_ISSUER_ID` | the App Store Connect API key's Issuer ID (Users and Access, Integrations, App Store Connect API) |
| `APPSTORE_KEY_ID` | the API key's Key ID |
| `APPSTORE_KEY_P8` | the contents of the key's `AuthKey_KEYID.p8`, as downloaded |

With `MACOS_P12_BASE64` set, both binaries are signed with the Developer ID,
with the hardened runtime and Apple's timestamp (`caboose-vmm` with its
virtualization entitlement, the launcher with none), and each is notarized
(zipped and submitted, waiting for Apple's verdict); the other four must
then be set too, and a signature or notarization that fails fails the
release, never falling back. A bare binary cannot be stapled: Gatekeeper
fetches the ticket online. Without it (a fork, or a repository not yet set
up) nothing changes: `caboose-vmm` is signed ad-hoc and the launcher keeps
the linker's signature. The workflow writes the secrets to files in
`RUNNER_TEMP` (0600, never printed) and removes them when the job ends,
whatever happened.
`rcodesign` 0.29 reads a `.p12` encrypted the classic way (3DES, as
Keychain Access exports it) and calls one in OpenSSL 3's default AES
"incorrect password": re-export that one with `openssl pkcs12 -legacy`.

Neither binary needs a hardened-runtime exception: they load only Apple's
system frameworks (through purego, whose callbacks are trampolines
compiled in, not code made at run time), set no `DYLD_` variable, and what
they run (docker, ssh, git, `caboose-vmm`) runs under its own signature.

`make vmm` does the same on a Mac when asked: `DEVID_P12` (the `.p12`'s
path) and `DEVID_P12_PASSWORD_FILE` (a file holding its password) sign it
with the Developer ID, and `APPSTORE_API_KEY_JSON` (from `rcodesign
encode-app-store-connect-api-key -o FILE ISSUER_ID KEY_ID AuthKey.p8`)
notarizes it too; this path needs `rcodesign` installed. Without them it
is ad-hoc, by `codesign`. A `caboose-vmm` already built is not signed
again: `rm caboose-vmm` first.

| | |
|---|---|
| `Dockerfile` | the default base image: OS packages, toolchains, jj |
| `layer.Dockerfile`, `layer-user.sh` | the layer on every base: the agent user, its home, the entrypoint |
| `imagecheck.sh` | the probe `caboose check-image` and every build run in the base; `internal/imagecheck` holds the requirements |
| `entrypoint.sh` | bootstraps Claude Code, clears stale state, prunes versions, runs `start.d`, idles |
| `shellrc.bash` | the `shell.d` loader, sourced from the `~/.bashrc` the layer sets up |
| `tmux.conf` | tmux set up to own no keys, baked into the image |
| `cmd/caboose`, `internal/` | host launcher, in Go: container lifecycle, mounts, tmux attach, image build |
| `embed.go` | the files embedded in the launcher, which is all the image build can see |
| `Makefile` | maintenance targets; builds the launcher into `./caboose` (gitignored) |
| `.goreleaser.yaml`, `.github/workflows/` | release build; CI |
| `macsign.sh` | signs the darwin binaries, ad-hoc or with a Developer ID, and notarizes them (goreleaser, `make vmm`) |
| `CLAUDE.md` | instructions for a session working on *this repo* |
| `sandbox/CLAUDE.md` | the global CLAUDE.md installed into the sandbox, for *every* project |
| `tests/run.sh` | integration suite |
| `tests/byo/` | bring-your-own-image suite and its test images (`make test-byo`) |

Note there is no state directory in this table: the checkout is source only.
