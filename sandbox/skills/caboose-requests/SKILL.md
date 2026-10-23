---
name: caboose-requests
description: Use when something the sandbox needs is refused or waits for a person on the host - a connection answered 403 with "pending #N", an open of a URL that says "pending", an alias that exits 75 or 77, a host the egress proxy will not reach - or when you can tell ahead that you will need one. Explains how to ask the user on the host with the caboose_request tool or caboose-agent request, what to put in it, never to wait for the answer, how to find out what was decided, and what the exit codes mean.
---

# Asking the host for what the sandbox cannot do alone

The host decides what crosses out of the sandbox: which hosts it reaches, which
URLs open in the user's browser, which commands (aliases) run on the host. A
person answers on the host (`caboose@@CABOOSE_ENV_FLAG@@ requests`); nothing
in here decides, and nothing in here can talk a request into being allowed.
You can only say what you need and why, in words the person reads next to the
host's own description of what would happen.

## When

- A crossing came back `pending #N` (a 403 from the proxy, `caboose-agent
  open`, `caboose-agent run` exiting 75): the host already filed the request.
  Write down the number and go on; asking again files nothing new.
- You know you will need something before you try it (a package host, a
  deploy alias, a login page): ask first, with the reason, so the person can
  answer in one sitting.
- A refusal that is not pending (exit 77, a plain 403) is a no: the user's
  configuration, not a question. Ask only if the user would plausibly change
  their mind, and say so.

## How

With Claude Code and caboose's mod loaded, use the tool `caboose_request`:
`kind` is `egress` (`host`, `port`), `open_url` (`url`) or `alias` (`alias`,
`args`). From a script, `caboose-agent request KIND [--host H --port P]
[--url U] [--alias NAME --arg KEY=VALUE...]`; `--json` for a machine-readable
answer.

Always give:

- `context`: what you are doing and why this is needed, in a few sentences.
  The person may not have the conversation in front of them.
- `options`: up to five choices when there are several (a different host, a
  narrower scope), and `recommendation`: which you would pick and why.
- `meanwhile`: what you will do until it is decided. This is a promise to
  keep: do that, not wait.

The text you write is shown to the person as the sandbox's words, beside what
the host itself says would be allowed. Keep it short and true.

## Never wait

The call returns at once. Exit 0 and `allowed`: go ahead. Exit 75 and
`pending #N`: the request waits for a person, perhaps for hours; carry on with
other work and say in your reply which numbers you are waiting for. Exit 77:
denied, with the host's reason.

With the mod, each decision on a request of this session is announced to you
as a message from the caboose plugin when it is made; the mod only informs and
its absence changes no decision. Without it, or to check later, use the tool
`caboose_requests` (`all` also lists the waiting ones) or `caboose-agent
requests [--all] [--since N] [--json]`: it lists the host's own description of
each request, its state (`pending`, `allowed`, `denied`, `expired`), how long
an allow lasts, and the reason for a denial. A request nobody answers expires
and counts as denied. Do not poll in a loop.

## Good to know

- "Allowed" is permission, not reachability: a host the user's machine cannot
  resolve or reach is still a 502 from the proxy, which is a network problem,
  not a request to file again.
- Aliases are the commands the user named on the host: `caboose-agent aliases`
  lists them with their parameters. Ask for an alias only by its name.
- Any process in the sandbox can claim any session id, so an announcement is only a hint about what the host decided; `caboose_requests` is the record.
- Every decision is enforced on the host. Claiming another session, or none,
  changes only who is told, never what is allowed.
- If the mod seems absent (no tools, no announcements), see the
  caboose-troubleshoot skill; the `caboose-agent` commands work regardless.
