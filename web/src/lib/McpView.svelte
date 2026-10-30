<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Grant, type McpInfo, type Rules, type Stack } from './api'

  let { stacks }: { stacks: Stack[] } = $props()

  let info = $state<McpInfo>()
  let error = $state('')
  let busy = $state(false)
  let now = $state(Date.now())
  // The grant being created (id empty), edited, or reactivated (ended)
  let form = $state<{ id: string; ended: boolean; label: string; minutes: number; rules: Rules }>()
  let created = $state<{ label: string; token: string }>()
  let dialog = $state<HTMLDialogElement>()
  let tokenDialog = $state<HTMLDialogElement>()

  const ALL = '*'
  const SHARED = 'shared'
  const scoped = $derived(info?.permissions.filter((p) => p.scoped) ?? [])
  const account = $derived(info?.permissions.filter((p) => !p.scoped) ?? [])
  const scopes = $derived([...stacks.map((s) => ({ id: s.id, name: s.name })), { id: SHARED, name: 'Shared networks & volumes' }])
  const active = $derived(info?.grants.filter(isActive) ?? [])
  const ended = $derived(info?.grants.filter((g) => !isActive(g)) ?? [])
  const durations = $derived(
    [15, 60, 120, 240, 480, 1440].filter((m) => m <= (info?.maxMinutes ?? 0)).concat(info && ![15, 60, 120, 240, 480, 1440].includes(info.maxMinutes) ? [info.maxMinutes] : []),
  )

  function isActive(g: Grant) {
    return !g.revoked && new Date(g.expires).getTime() > now
  }

  async function load() {
    try {
      info = await api.mcp()
      error = ''
    } catch (e: any) {
      error = e.message
    }
  }

  /* Rules: permission -> scopes. "*" is every stack, now and later, and the shared files. */

  const has = (perm: string, scope: string) => !!form?.rules[perm]?.some((s) => s === ALL || s === scope)

  function set(perm: string, scope: string, on: boolean) {
    const r = form!.rules
    let list = (r[perm] ?? []).filter((s) => s !== scope)
    if (scope === ALL) list = []
    if (on) list.push(scope)
    if (list.length) r[perm] = list
    else delete r[perm]
  }

  // A column or row header turns the whole line on, or off when it is all on already
  function toggleColumn(perm: string) {
    set(perm, ALL, !has(perm, ALL))
  }

  function toggleRow(scope: string) {
    const on = !scoped.every((p) => has(p.id, scope))
    for (const p of scoped) if (!form!.rules[p.id]?.includes(ALL)) set(p.id, scope, on)
  }

  const READ = ['stacks.read', 'files.read', 'history.read', 'logs.read', 'resources.read', 'images.read', 'secrets.read']
  const OPERATE = [...READ, 'units.start', 'units.stop', 'units.restart', 'images.pull']
  function preset(perms: string[] | 'all') {
    const ids = perms === 'all' ? info!.permissions.map((p) => p.id) : perms
    form!.rules = Object.fromEntries(ids.map((id) => [id, [ALL]]))
  }

  function open(g?: Grant) {
    form = g
      ? {
          id: g.id,
          ended: !isActive(g),
          label: g.label,
          minutes: isActive(g) ? 0 : durations.includes(60) ? 60 : durations[0],
          rules: structuredClone($state.snapshot(g.rules)),
        }
      : { id: '', ended: false, label: 'Claude', minutes: durations.includes(60) ? 60 : durations[0], rules: {} }
    error = ''
    queueMicrotask(() => dialog?.showModal())
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    busy = true
    error = ''
    try {
      const body = { label: form!.label, minutes: form!.minutes, rules: $state.snapshot(form!.rules) }
      if (form!.id) {
        await api.updateGrant(form!.id, body)
      } else {
        const r = await api.createGrant(body)
        created = { label: r.grant.label, token: r.token }
        queueMicrotask(() => tokenDialog?.showModal())
      }
      dialog?.close()
      await load()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  async function end(g: Grant) {
    if (!confirm(`End "${g.label}" now? Access stops immediately, also in running sessions. The token is kept, so you can reactivate the grant later.`)) return
    try {
      await api.endGrant(g.id)
      await load()
    } catch (e: any) {
      error = e.message
    }
  }

  async function remove(g: Grant) {
    if (!confirm(`Delete "${g.label}" and its token for good? Clients using it need a new grant and token.`)) return
    try {
      await api.deleteGrant(g.id)
      await load()
    } catch (e: any) {
      error = e.message
    }
  }

  function remaining(t: string) {
    const ms = new Date(t).getTime() - now
    if (ms <= 0) return 'ended'
    const m = Math.ceil(ms / 60000)
    return m < 60 ? `${m} min left` : `${Math.floor(m / 60)} h ${m % 60} min left`
  }

  const fmtDur = (m: number) => (m < 60 ? `${m} minutes` : m % 60 ? `${(m / 60).toFixed(1)} hours` : `${m / 60} hour${m === 60 ? '' : 's'}`)
  const fmt = (t?: string) => (t ? new Date(t).toLocaleString() : '')
  const scopeName = (id: string) => (id === ALL ? 'all' : id === SHARED ? 'shared' : (stacks.find((s) => s.id === id)?.name ?? id))
  const permLabel = (id: string) => info?.permissions.find((p) => p.id === id)?.label ?? id

  function summary(g: Grant) {
    const parts = Object.entries(g.rules).map(([p, s]) => `${permLabel(p)}${s.includes(ALL) ? '' : ' (' + s.map(scopeName).join(', ') + ')'}`)
    return parts.length ? parts.join(' · ') : 'nothing'
  }

  const claudeCmd = $derived(
    created && info
      ? `claude mcp add --scope user --transport http ${info.serverName} ${info.endpoint} --header "Authorization: Bearer ${created.token}"`
      : '',
  )
  const jsonCfg = $derived(
    created && info
      ? JSON.stringify({ mcpServers: { [info.serverName]: { type: 'http', url: info.endpoint, headers: { Authorization: 'Bearer ' + created.token } } } }, null, 2)
      : '',
  )

  onMount(() => {
    load()
    const tick = setInterval(() => (now = Date.now()), 10000)
    const refresh = setInterval(() => document.visibilityState === 'visible' && !form && load(), 15000)
    return () => {
      clearInterval(tick)
      clearInterval(refresh)
    }
  })
</script>

<section>
  <div class="row wrap head">
    <h1 class="grow">AI access</h1>
    <button class="primary" disabled={!info?.enabled} onclick={() => open()}>+ New grant</button>
  </div>
  <p class="muted">
    An MCP endpoint for AI assistants. It is <b>off</b> while no grant is active. A grant has its own token, lasts at most
    {info ? fmtDur(info.maxMinutes) : '…'} at a time, and allows exactly what you tick, per stack. Tools it does not allow are
    not offered at all. Changes apply to running sessions at once. <b>End now</b> keeps the token, so <b>Reactivate</b> lets
    the same client back in; <b>Delete</b> drops it. Every call is logged below with the address it came from.
  </p>
  {#if info && !info.enabled}
    <div class="error-box">MCP access is turned off on this server (QUADSTACK_MCP_MAX_HOURS=0).</div>
  {/if}
  {#if error && !form}<div class="error-box">{error}</div>{/if}

  {#if info}
    <p class="small">Endpoint: <code>{info.endpoint}</code> · server name <code>{info.serverName}</code>
      {#if active.length}<span class="badge on">on: {active.length} active grant{active.length > 1 ? 's' : ''}</span>{:else}<span class="badge">off</span>{/if}
    </p>

    <div class="panel scroll">
      <table>
        <thead><tr><th>Grant</th><th>Allows</th><th>Ends</th><th>Last used</th><th></th></tr></thead>
        <tbody>
          {#each active as g (g.id)}
            <tr>
              <td><b>{g.label}</b><div class="muted small">by {g.createdBy}</div></td>
              <td class="small allows">{summary(g)}</td>
              <td class="small nowrap">{remaining(g.expires)}<div class="muted">{fmt(g.expires)}</div></td>
              <td class="small muted nowrap">{fmt(g.lastUsed) || 'never'}{#if g.lastFrom}<div>from {g.lastFrom}</div>{/if}</td>
              <td class="actions">
                <button onclick={() => open(g)}>Edit</button>
                <button class="danger" onclick={() => end(g)}>End now</button>
              </td>
            </tr>
          {:else}
            <tr><td colspan="5" class="muted">No active grant: the endpoint refuses every request.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>

    {#if ended.length}
      <details>
        <summary class="muted">Ended grants ({ended.length}): their tokens still exist, so reactivating one lets its client back in</summary>
        <div class="panel scroll">
          <table>
            <tbody>
              {#each ended as g (g.id)}
                <tr>
                  <td>{g.label}</td>
                  <td class="small muted allows">{summary(g)}</td>
                  <td class="small muted nowrap">
                    {g.revoked ? 'ended ' + fmt(g.revoked) : 'expired ' + fmt(g.expires)}
                    {#if g.lastFrom}<div>last from {g.lastFrom}</div>{/if}
                  </td>
                  <td class="actions">
                    <button class="primary" disabled={!info.enabled} onclick={() => open(g)}>Reactivate</button>
                    <button class="ghost danger" title="Delete grant and token" onclick={() => remove(g)}>🗑</button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </details>
    {/if}

    <h2>Audit log</h2>
    <div class="panel scroll">
      <table>
        <thead><tr><th>Time</th><th>Grant</th><th>From</th><th>Tool</th><th>Arguments</th><th>Result</th></tr></thead>
        <tbody>
          {#each info.audit as e, i (i)}
            <tr>
              <td class="small muted nowrap">{fmt(e.time)}</td>
              <td class="small">{e.label}</td>
              <td class="small muted nowrap">{e.from ?? ''}</td>
              <td><code>{e.tool}</code></td>
              <td class="small"><code class="args">{e.args ? JSON.stringify(e.args) : ''}</code></td>
              <td class="small">
                <span class={'badge ' + e.result}>{e.result}</span>
                {#if e.error}<div class="muted err">{e.error}</div>{/if}
              </td>
            </tr>
          {:else}
            <tr><td colspan="6" class="muted">No calls yet.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

{#if form && info}
  <dialog class="wide" bind:this={dialog} onclose={() => ((form = undefined), (error = ''))}>
    <h2>{form.ended ? `Reactivate "${form.label}"` : form.id ? `Edit "${form.label}"` : 'New grant'}</h2>
    {#if form.ended}<p class="muted small">The grant keeps its token: a client that used it before is let back in for the time you choose.</p>{/if}
    <form onsubmit={submit} autocomplete="off">
      <div class="row wrap">
        <label class="field grow">
          Name
          <input bind:value={form.label} required maxlength="80" />
        </label>
        <label class="field">
          {form.id && !form.ended ? 'Ends' : 'Lasts'}
          <select bind:value={form.minutes}>
            {#if form.id && !form.ended}<option value={0}>keep the current end</option>{/if}
            {#each durations as m}<option value={m}>{form.id && !form.ended ? fmtDur(m) + ' from now' : fmtDur(m)}</option>{/each}
          </select>
        </label>
      </div>

      <div class="row wrap presets">
        <span class="muted small">Start from:</span>
        <button type="button" onclick={() => (form!.rules = {})}>Nothing</button>
        <button type="button" onclick={() => preset(READ)}>Read only</button>
        <button type="button" onclick={() => preset(OPERATE)}>Read &amp; operate</button>
        <button type="button" onclick={() => preset('all')}>Everything</button>
      </div>

      <h3>Per stack</h3>
      <p class="muted small">Click a column or row name to switch it all. <b>All stacks</b> also covers stacks created later.</p>
      <div class="matrix">
        <table>
          <thead>
            <tr>
              <th></th>
              {#each scoped as p (p.id)}
                <th class="col"><button type="button" class="ghost" title={p.help} onclick={() => toggleColumn(p.id)}>{p.label}</button></th>
              {/each}
            </tr>
          </thead>
          <tbody>
            <tr class="all">
              <th><b>All stacks</b></th>
              {#each scoped as p (p.id)}
                <td><input type="checkbox" title={p.help} checked={has(p.id, ALL)} onchange={(e) => set(p.id, ALL, e.currentTarget.checked)} /></td>
              {/each}
            </tr>
            {#each scopes as sc (sc.id)}
              <tr>
                <th><button type="button" class="ghost rowname" onclick={() => toggleRow(sc.id)}>{sc.name}</button></th>
                {#each scoped as p (p.id)}
                  <td>
                    <input
                      type="checkbox"
                      title={`${p.label} on ${sc.name}: ${p.help}`}
                      checked={has(p.id, sc.id)}
                      disabled={!!form.rules[p.id]?.includes(ALL)}
                      onchange={(e) => set(p.id, sc.id, e.currentTarget.checked)}
                    />
                  </td>
                {/each}
              </tr>
            {/each}
          </tbody>
        </table>
      </div>

      <h3>Whole account</h3>
      <div class="account">
        {#each account as p (p.id)}
          <label class="check">
            <input type="checkbox" checked={has(p.id, ALL)} onchange={(e) => set(p.id, ALL, e.currentTarget.checked)} />
            <span><b>{p.label}</b> <span class="muted">{p.help}</span></span>
          </label>
        {/each}
      </div>

      <details class="help">
        <summary class="muted small">What each column allows</summary>
        <dl>
          {#each scoped as p (p.id)}<dt>{p.label}</dt><dd>{p.help}</dd>{/each}
        </dl>
        <p class="muted small">
          A file belongs to the stack it is in, both before and after a change: a top-level container that sets
          <code>Pod=</code> joins the pod's stack, so moving it needs Write on both. Exec is only offered while the browser
          terminal is enabled (QUADSTACK_TERMINAL).
        </p>
      </details>

      {#if error}<div class="error-box">{error}</div>{/if}
      <div class="row foot">
        <span class="muted small">{Object.keys(form.rules).length} permission{Object.keys(form.rules).length === 1 ? '' : 's'}</span>
        <span class="grow"></span>
        <button type="button" onclick={() => dialog?.close()}>Cancel</button>
        <button class="primary" type="submit" disabled={busy}>{form.ended ? 'Reactivate' : form.id ? 'Save' : 'Create grant'}</button>
      </div>
    </form>
  </dialog>
{/if}

{#if created}
  <dialog class="wide" bind:this={tokenDialog} onclose={() => (created = undefined)}>
    <h2>Token for "{created.label}"</h2>
    <p class="muted">
      Shown once; Quadstack keeps only a hash. You set this up once per machine: ending and reactivating the grant keeps the
      token. The server is named <code>{info?.serverName}</code> after this machine, so several machines sit side by side.
      For a new token on the same machine, run <code>claude mcp remove {info?.serverName}</code> first.
    </p>
    <label class="field">Claude Code<textarea readonly rows="3" onfocus={(e) => e.currentTarget.select()}>{claudeCmd}</textarea></label>
    <label class="field">Other MCP clients (JSON)<textarea readonly rows="9" onfocus={(e) => e.currentTarget.select()}>{jsonCfg}</textarea></label>
    <div class="row foot">
      <span class="grow"></span>
      <button onclick={() => navigator.clipboard?.writeText(created!.token)}>Copy token</button>
      <button class="primary" onclick={() => tokenDialog?.close()}>Done</button>
    </div>
  </dialog>
{/if}

<style>
  h1 {
    margin: 0;
    font-size: 20px;
  }
  h2 {
    margin: 22px 0 8px;
    font-size: 15px;
  }
  h3 {
    margin: 16px 0 4px;
    font-size: 14px;
  }
  .head {
    margin-bottom: 8px;
  }
  .scroll {
    overflow-x: auto;
  }
  .small {
    font-size: 12.5px;
  }
  .nowrap {
    white-space: nowrap;
  }
  .allows {
    max-width: 420px;
  }
  .actions {
    text-align: right;
    white-space: nowrap;
  }
  .badge.on,
  .badge.ok {
    background: color-mix(in srgb, var(--ok) 18%, transparent);
    color: var(--ok);
  }
  .badge.denied {
    background: color-mix(in srgb, var(--warn) 20%, transparent);
    color: var(--warn);
  }
  .badge.error {
    background: color-mix(in srgb, var(--bad) 16%, transparent);
    color: var(--bad);
  }
  .args {
    word-break: break-all;
    font-size: 11.5px;
  }
  .err {
    max-width: 320px;
  }
  details {
    margin-top: 10px;
  }
  details .panel {
    margin-top: 6px;
  }
  .presets {
    gap: 6px;
    margin-bottom: 4px;
  }
  .matrix {
    overflow: auto;
    max-height: 46vh;
    border: 1px solid var(--border);
    border-radius: 6px;
  }
  .matrix th,
  .matrix td {
    padding: 3px 6px;
    text-align: center;
  }
  .matrix thead th {
    position: sticky;
    top: 0;
    background: var(--panel);
    z-index: 1;
  }
  .matrix tbody th {
    text-align: left;
    white-space: nowrap;
    font-size: 13px;
    color: var(--text);
  }
  .matrix .col button,
  .matrix .rowname {
    padding: 2px 4px;
    font-size: 12px;
  }
  .matrix .rowname {
    font-size: 13px;
  }
  .matrix tr.all {
    background: var(--panel-2);
  }
  .account {
    display: grid;
    gap: 6px;
  }
  .check {
    display: flex;
    gap: 8px;
    align-items: baseline;
    font-size: 13px;
  }
  .help dl {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 2px 12px;
    font-size: 12.5px;
  }
  .help dt {
    font-weight: 600;
  }
  .help dd {
    margin: 0;
  }
  textarea {
    width: 100%;
    font-size: 12px;
  }
  .foot {
    margin-top: 14px;
  }
</style>
