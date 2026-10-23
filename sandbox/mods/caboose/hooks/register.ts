// caboose's mod for Claude Code, loaded in a caboose sandbox through
// CLAUDE_CODE_PLUGIN_DIRS. It gives the model two tools to ask the host for
// something the sandbox needs (a host to reach, an alias to run, a URL to
// open) and says what the host decided. It informs only: the host enforces
// its decisions whatever this module does or does not do.

import type { EngineInterface, Register } from 'claude-code'

const AGENT = 'caboose-agent'
const WAIT_SECONDS = 240
const POLL_TIMEOUT_MS = 300_000
const BACKOFF_MIN_MS = 5_000
const BACKOFF_MAX_MS = 300_000
const MAX_TEXT = 300
const MAX_REMEMBERED = 500

const REQUEST_HELP =
  'Asks the person on the host for something this sandbox may not do on its own: ' +
  'kind "egress" (host, port), "open_url" (url) or "alias" (alias, args). ' +
  'It never waits for an answer. If the result says pending, note the request number, ' +
  'say in "meanwhile" what you will do without it, and carry on; the decision is ' +
  'announced here when it is made. Put what the person needs to decide in "context", ' +
  'up to five choices in "options" and yours in "recommendation".'

// Everything the host sends is printed through here: control and format
// characters out, one line, bounded.
const clean = (s: unknown, max = MAX_TEXT): string =>
  String(s ?? '')
    .replace(/[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u2028-\u202f\u2060-\u206f\ufeff\ufff9-\ufffb]/g, ' ')
    .replace(/ +/g, ' ')
    .trim()
    .slice(0, max)

const asText = (v: unknown): string | undefined =>
  typeof v === 'string' && v !== '' ? v : typeof v === 'number' ? String(v) : undefined

type Info = {
  id: number
  state: string
  scope?: string
  hours?: number
  effect?: string
  reason?: string
}

// What the host decided, as one line: host fields only.
const announce = (q: Info): string | undefined => {
  const what = `caboose: request #${q.id} (${clean(q.effect, 120) || 'no description'})`
  if (q.state === 'allowed') {
    const how =
      q.scope === 'always' ? ' always' : q.scope === 'hours' ? ` for ${Number(q.hours) || 0} hours` : q.scope === 'once' ? ' once' : ''
    return clean(`${what} was allowed${how} on the host.`)
  }
  if (q.state === 'denied') return clean(`${what} was denied on the host: ${clean(q.reason, 120) || 'no reason given'}`)
  if (q.state === 'expired') return clean(`${what} expired unanswered, so it is denied.`)
  return undefined
}

const run = ($: EngineInterface, args: string[]) => $.process.run([AGENT, ...args], { timeoutMs: POLL_TIMEOUT_MS })

// The result is the command's JSON, as text: a plugin tool's result is a
// string or content blocks, never an object. When it printed none, why not.
const outcome = (r: { exitCode: number; stdout: string; stderr: string }) => {
  try {
    return { result: JSON.stringify(JSON.parse(r.stdout)) }
  } catch {
    return { result: clean(r.stderr || r.stdout || `exit ${r.exitCode}`, 600), isError: true as const }
  }
}

// Each decision on this session's requests, told once as the plugin's own
// words (no asUser: the engine frames the text as the plugin's).
const watch = async ($: EngineInterface, isStopped: () => boolean) => {
  let cursor = -1
  let backoff = BACKOFF_MIN_MS
  const told: string[] = []
  while (!isStopped()) {
    try {
      const id = await $.session.id()
      if (cursor < 0) {
        // 0 would list decisions as if none had been made yet: start from 1.
        const r = await run($, ['requests', '--since', 'now', '--json', '--session', id])
        cursor = Math.max(1, Number(JSON.parse(r.stdout).cursor) || 0)
      }
      const r = await run($, ['requests', '--wait', String(WAIT_SECONDS), '--since', String(cursor), '--session', id, '--mod', '--json'])
      const out = JSON.parse(r.stdout) as { cursor: number; requests: Info[] }
      cursor = Math.max(cursor, Number(out.cursor) || 0)
      backoff = BACKOFF_MIN_MS
      for (const q of [...out.requests].reverse()) {
        const text = announce(q)
        const key = `${q.id}:${q.state}`
        if (text === undefined || told.includes(key)) continue
        told.push(key)
        if (told.length > MAX_REMEMBERED) told.shift()
        await $.prompt.submit({ text })
      }
    } catch {
      await $.clock.sleep(backoff)
      backoff = Math.min(backoff * 2, BACKOFF_MAX_MS)
    }
  }
}

export const register: Register = on => {
  let stopped = false
  // session.start may fire again (enable, respawn, reload): only the newest loop runs.
  let generation = 0

  on('session.start', async ($, e, next) => {
    await $.tool.register({
      name: 'caboose_request',
      description: REQUEST_HELP,
      isDeferred: false,
      inputSchema: {
        type: 'object',
        properties: {
          kind: { type: 'string', enum: ['egress', 'open_url', 'alias'] },
          host: { type: 'string' },
          port: { type: 'integer' },
          url: { type: 'string' },
          alias: { type: 'string' },
          args: { type: 'object', additionalProperties: { type: 'string' } },
          context: { type: 'string' },
          options: { type: 'array', items: { type: 'string' }, maxItems: 5 },
          recommendation: { type: 'string' },
          meanwhile: { type: 'string' },
        },
        required: ['kind'],
      },
    })
    await $.tool.register({
      name: 'caboose_requests',
      description: 'Lists the requests of this session and what the host decided. Set "all" to include the ones still waiting.',
      isDeferred: false,
      inputSchema: { type: 'object', properties: { all: { type: 'boolean' } } },
    })
    // The loop outlives this hook; it ends with the session.
    const mine = ++generation
    void watch($, () => stopped || mine !== generation)
    return next(e)
  })

  on('tool.call', { tool: 'mcp__caboose__caboose_request' }, async ($, e) => {
    const a = e as unknown as Record<string, unknown>
    const argv = ['request', String(a.kind ?? ''), '--json', '--session', await $.session.id()]
    for (const [flag, key] of [['--host', 'host'], ['--port', 'port'], ['--url', 'url'], ['--alias', 'alias'], ['--context', 'context'], ['--recommend', 'recommendation'], ['--meanwhile', 'meanwhile']] as const) {
      const v = asText(a[key])
      if (v !== undefined) argv.push(flag, v)
    }
    if (Array.isArray(a.options)) for (const o of a.options) if (typeof o === 'string') argv.push('--option', o)
    if (a.args && typeof a.args === 'object')
      for (const [k, v] of Object.entries(a.args as Record<string, unknown>)) argv.push('--arg', `${k}=${String(v)}`)
    return outcome(await run($, argv))
  })

  on('tool.call', { tool: 'mcp__caboose__caboose_requests' }, async ($, e) => {
    const a = e as unknown as Record<string, unknown>
    const argv = ['requests', '--json', '--session', await $.session.id()]
    if (a.all === true) argv.push('--all')
    return outcome(await run($, argv))
  })

  // A /clear or a resume ends the conversation but not the process, and no
  // session.start follows: the loop goes on under the new session's id.
  on('session.end', ($, e, next) => {
    if (e.reason !== 'clear' && e.reason !== 'resume') stopped = true
    return next(e)
  })
}
