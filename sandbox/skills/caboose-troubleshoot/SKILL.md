---
name: caboose-troubleshoot
description: Use when something about the caboose sandbox itself misbehaves - "no host is linked", a port not reachable from the host's browser, caboose-agent failing, a host edit a file watcher or dev server does not see, a file that seems missing but should exist, the SSH agent or git push/signing not working, outbound connections refused, timing out or not reaching a VPN host (HTTP 403/502/503 from the proxy, DNS failing), docker not working in the sandbox, a start.d script that did not run, a launch saying the sandbox comes from an incompatible caboose, or work that stalled while the Mac slept. Explains causes per isolation and what the user runs on the host.
---

# When the sandbox misbehaves

First, have the user run `caboose@@CABOOSE_ENV_FLAG@@ doctor` on the host: it
checks the whole environment and names each problem with the command that
fixes it. `caboose@@CABOOSE_ENV_FLAG@@ status` shows the sandbox's state
(isolation, image, keeps, SSH agent, host commands, docker). This sandbox is
`@@CABOOSE_PROFILE@@`, image `@@CABOOSE_IMAGE@@`.

## The host link

Ports, `caboose-agent open|notify|host` and proposal checks go through the
link: `caboose-agent` in here, `caboose@@CABOOSE_ENV_FLAG@@ link` on the host.
@@IF isolation=gvisor@@
So does the relay of host edits to file watchers.
@@END@@
@@IF isolation=vm@@
So do the relay of host edits, outbound connections and the SSH agent.
@@END@@
Every launch starts it, and it reconnects by itself.
"no host is linked to this sandbox right now" means it is not running: any
`caboose@@CABOOSE_ENV_FLAG@@` launch starts it, or
`caboose@@CABOOSE_ENV_FLAG@@ link --restart` without a session. Its log is
`link.log` in the environment's data dir on the host.

A port not reaching the host: `caboose-agent ports` says, for each listening
port, whether it is forwarded, not in `forward_ports` (`[link]` of
`config.toml`), or already in use on the host.
@@IF isolation=gvisor,vm@@

## Host edits and file watchers

The host's edits under the roots do not become inotify events in here by
themselves. The link watches the roots on the host and raises an event for
each changed path by setting its mode to what it already is: an
`IN_ATTRIB`, not `IN_MODIFY`.

- Watchers that take any event as a change (Node's `fs.watch` and the tools
  on it) see it. One that ignores attribute changes (Go's fsnotify reports
  them as `Chmod`, which some tools skip) does not: use the tool's polling
  mode.
- A deleted file shows only as its directory changing.
- Nothing inside a `.git` directory is relayed.
- Bursts collapse: over 64 changes in one directory arrive as the
  directory, over 4096 at once as the roots.
- Nothing is relayed while the link is down.
@@END@@
@@IF isolation=gvisor@@

## Files that seem missing (gVisor)

On OrbStack a directory read again through a handle that stays open can
list what it held the first time. caboose's `runsc` runs with `--dcache=0`,
which fixes new directories, but a directory some process keeps open (a
shell's working directory) can still list stale contents. Before taking a
file for gone, `stat` it by name.
@@END@@
@@IF isolation=vm@@

## Outbound connections (vm)

The VM has two ways out:

- **The host's proxy**, with the vm profile's `egress = true` (the default):
  `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` are
  `http://127.0.0.1:9128`, served by `caboose-agent` while the link is up.
  The Mac resolves and dials, so its VPN and DNS apply. `NO_PROXY` keeps
  `localhost`, loopback, `.localhost`, `172.16.0.0/12` (the inner dockerd's
  bridges) and the VM's hostname off it. `caboose-agent wait-proxy` waits
  until it serves.
- **The VM's own NAT**, for everything else: clients that ignore the proxy
  variables, raw TCP and UDP, and every DNS lookup. NAT reaches no VPN, so a
  VPN-only name fails `nslookup` yet works with `curl` through the proxy.

With the link down nothing listens on `127.0.0.1:9128`: connections are
refused outright. Otherwise the proxy answers, with the host's reason as the
body:

| Code | Meaning | What to do |
|---|---|---|
| 403 | a port outside `egress_ports` (default 22, 80, 443), or an address that is not public (private, LAN, tailnet, loopback, link-local, the Mac's own) | the user adds the name, CIDR or port to `egress_allow` / `egress_ports` in `[vm.NAME]`; loopback, link-local and the Mac's own addresses pass only for an explicit address or CIDR |
| 502 | the name does not resolve on the Mac, or the host does not answer | check the name; the Mac's VPN |
| 503 | too many connections, or the link failed mid-request | retry; see the host link above |

`egress_ports` and `egress_allow` apply within seconds; `egress` itself
takes `caboose@@CABOOSE_ENV_FLAG@@ restart`.

ssh goes through the proxy (`ProxyCommand /run/caboose/agent connect %h %p`,
from `/etc/ssh/ssh_config.d/50-caboose-egress.conf`) only when the image's
ssh is 9.6 or later, or a Debian 12 or Ubuntu 22.04 build with the
CVE-2023-51385 fix; otherwise ssh stays on NAT and the VM's console
(`caboose@@CABOOSE_ENV_FLAG@@ logs`) says why. An image whose ssh config does not include
`ssh_config.d` gets `GIT_SSH_COMMAND` instead, so git's ssh uses the proxy
and plain `ssh` does not. A `ProxyCommand` or `ProxyJump` in `~/.ssh/config`
wins.

Containers run by the VM's dockerd get none of this: they stay on NAT.
@@END@@

## SSH agent and git

`ssh-add -l` lists the keys the sandbox can use; the keys themselves stay on
the host.
@@IF isolation=vm@@
Under vm the link carries the agent: with the link down, `SSH_AUTH_SOCK`
answers nothing.
@@END@@
@@IF isolation=gvisor@@
Under gVisor the host's `runsc` needs `--host-uds=open` to reach the agent's
socket; `caboose@@CABOOSE_ENV_FLAG@@ doctor` says how to add it (then `caboose@@CABOOSE_ENV_FLAG@@ restart`).
@@END@@
@@IF isolation=container,gvisor@@
On a Mac, OrbStack and Docker Desktop forward the agent they were started
with, not the terminal's; a change takes restarting the engine, then
`caboose@@CABOOSE_ENV_FLAG@@ restart`.
@@END@@
`ssh_agent` in `[link]` of `config.toml` names the agent outright: under vm
it applies within seconds; on Linux under container or gvisor it is a mount,
so it takes `caboose@@CABOOSE_ENV_FLAG@@ restart`; OrbStack and Docker Desktop
ignore it. Commit
signing is set up by `caboose@@CABOOSE_ENV_FLAG@@ setup git`.

## Docker in the sandbox

@@IF isolation=vm@@
The VM runs its own `dockerd` when the image has it (`caboose@@CABOOSE_ENV_FLAG@@ status`, its
`dockerd` line; log `/var/log/caboose-dockerd.log`). Its images are on a kept
disk; `caboose@@CABOOSE_ENV_FLAG@@ prune --docker` on the host empties it, after asking.
@@ELSE@@
There is no daemon in here. A docker CLI talks to nothing unless the profile
sets `engine_socket = true`, which mounts the host engine's socket and makes
the sandbox root-equivalent on the host: the user's decision, never a casual
fix.
@@END@@

## Stalls

On a Mac, the sandbox
@@IF isolation=vm@@
(the VM)
@@ELSE@@
(the engine's VM)
@@END@@
is paused while the Mac sleeps: background work makes no progress, and
resumes on wake.

## After an update

A launcher newer than the sandbox keeps working with it. If a launch says the
sandbox comes from an incompatible caboose, `caboose@@CABOOSE_ENV_FLAG@@ restart`
moves it onto the new image (ending every session).
