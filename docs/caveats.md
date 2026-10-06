# Caveats

- On macOS the Docker VM is suspended while the laptop sleeps, so background
  agents make no progress. If that matters, run the same image on an
  always-on host.
- Old versions are pruned at container start and on `caboose prune`, not the
  moment an update lands — start-up is when no session holds a binary open.
- `.claude.json` is the one single-file bind mount, and it is pinned to
  the inode it was created from. A tool on the host that saves by writing a
  temp file and renaming it over the original — as many editors do — gives
  the host file a new inode and leaves the container's mount pointing at a
  deleted one, which needs `caboose restart` to repair. Edit it in
  place, or from inside the container.
- The docker **CLI** is in the default image but has no socket to talk to,
  so it is inert. `engine_socket = true` in a `container` or `gvisor` profile mounts the host's socket, and you should
  understand what that means before doing it: anything that reaches
  `/var/run/docker.sock` can run `docker run -v /:/host --privileged` and
  own the host as root. The container reads untrusted input all day (repo
  contents, web pages, MCP responses), so the realistic risk is an
  injection escalating out of the container, not the model going rogue.
  Note it does **not** let the test suite run from inside either — the
  suite restarts the container on its third line and would kill the session
  running it. Point `DOCKER_HOST` at a socket proxy instead if you only want
  read-only endpoints. `caboose status` always says which side of this line a
  container is on.
- A caboose checkout under a root is, like any other checkout
  there, **writable** from inside — and its `CLAUDE.md` invites the agent to
  edit the launcher's source and the `Dockerfile`, which the *host* then
  builds and executes. That is the price of being able to fix the sandbox
  from within it; keep the checkout outside every root if you would
  rather not pay it, and it becomes invisible from inside. An installed
  binary with no checkout has nothing there to edit.
- [Updates](getting-started.md#updates) are checked against the `checksums.txt` of the same
  release, which catches a download cut short or damaged, not a release
  someone else published: releases are not signed yet, so an install
  trusts whatever the GitHub release holds, as `install.sh` itself does.
  `CABOOSE_NO_AUTO_UPDATE=1` and a pinned `CABOOSE_VERSION` keep an install
  where it is.
