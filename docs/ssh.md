# SSH agent and commit signing

The host's SSH agent is forwarded into the container, so git and ssh there
push, and sign commits, with keys that never enter the sandbox. `caboose
status` shows what the sandbox makes of it (`ssh agent : forwarded, 2
keys`), and a new container says so when the agent it got is empty or
unreachable. `caboose doctor` checks it too, along with whether the key the
sandbox signs with is one the agent holds.

On a Mac, Docker runs in a VM, and a Mac socket bind-mounted into it does
not arrive as a socket. OrbStack and Docker Desktop provide their own
forwarded socket, `/run/host-services/ssh-auth.sock`, and with either one
that is what caboose mounts. It is the only way a third-party agent such as
**1Password**'s reaches a container. The engine forwards the agent it was
started with, so 1Password's has to be the `SSH_AUTH_SOCK` of apps started
outside a terminal: 1Password documents that as ["Configure SSH_AUTH_SOCK
globally"](https://www.1password.dev/ssh/agent/compatibility/)
(a launch agent), after which the engine needs a restart. With another
engine on a Mac, the host's `$SSH_AUTH_SOCK` is mounted when it is a
socket, which works for macOS's own agent at most. On Linux the agent's
socket is mounted directly: the one `ssh` itself would use, so an
`IdentityAgent` in `~/.ssh/config` (1Password's setup there) wins over
`$SSH_AUTH_SOCK`. The mount is fixed when the container is created, so a
change to any of this needs `caboose restart`.

Under `isolation = "vm"` nothing of the Mac's can be mounted into the
VM, so the [host link](host-link.md) carries the agent instead: the agent
in the sandbox listens at the same path, and each client there reaches the
agent `ssh` on the Mac would use, as the launch that started the link saw
it: an `IdentityAgent` in `~/.ssh/config`, else the terminal's
`$SSH_AUTH_SOCK`. The link starts with a session; `caboose link
--restart` starts one without.

**`ssh_agent`** in `config.toml` (or `CABOOSE_SSH_AGENT`) names the
agent outright, whatever the terminal says, or `none` for none. It is for
the Mac where something else holds `$SSH_AUTH_SOCK` -- a work login that
loads its own certificate, say -- while ssh and git there never read it
(remotes over HTTPS, signing through 1Password's `op-ssh-sign`), so
nothing on the Mac shows the agent is not yours:

    ssh_agent = "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"

The link rereads `config.toml`, so it takes effect without a restart. It
applies under `vm` and, on Linux, to the socket mounted under `docker` and
`gvisor`; with OrbStack or Docker Desktop the engine forwards its own, and
`caboose doctor` says the setting is not used. `caboose doctor` names the
agent the sandbox gets and what chose it.

**Commit signing** with an SSH key is set up by `caboose setup git`, which
offers the host's key when the host signs with SSH (`gpg.format = ssh`),
and writes `gpg.format`, the key and `commit.gpgsign` into the sandbox's
git config. Not `gpg.ssh.program`: on a Mac that is 1Password's
`op-ssh-sign`, which does not exist in the container. git's default,
`ssh-keygen`, signs there through the forwarded agent instead, and
1Password asks you to approve each signature on the Mac as it does there.
The key goes in as the literal public key; a `user.signingkey` (or a
pasted path) that names a `.pub` file is read for it, and a private key
file is refused (that would mean copying the key in). The image needs
`ssh-keygen` for this, part of any OpenSSH client (the default image has
one).
