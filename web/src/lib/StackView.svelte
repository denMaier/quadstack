<script lang="ts">
  import { api, wsURL, type Me, type Stack } from './api'
  import { unitBadge } from './quadlet'
  import FileEditor from './FileEditor.svelte'
  import NewFileDialog from './NewFileDialog.svelte'
  import Terminal from './Terminal.svelte'
  import { catalog } from './catalog.svelte'

  let { stack, me, refresh }: { stack: Stack; me?: Me; refresh: () => Promise<void> } = $props()

  let busy = $state('')
  let error = $state('')
  let file = $state('')
  let newFileOpen = $state(false)
  let reconnect = $state(0)
  // What the terminal panel shows: the stack's logs, one unit's logs, an image pull or a shell
  let panel = $state<{ kind: 'logs' | 'pull' | 'exec'; title: string; url: string; interactive?: boolean }>()

  const wiring = $derived(stack.files.filter((f) => f.networks?.length || f.mounts?.length || f.secrets?.length))
  const isResource = (n: string) => catalog.resources.some((r) => r.file.path.split('/').pop() === n || r.file.name === n)
  const units = $derived(stack.files.filter((f) => f.unit && ['container', 'pod', 'kube'].includes(f.kind)).map((f) => f.unit!))

  // Reset the view when switching stacks
  let shownStack = ''
  $effect(() => {
    if (stack.id === shownStack) return
    shownStack = stack.id
    error = ''
    file = stack.files.find((f) => f.kind === 'pod' || f.kind === 'container')?.path ?? stack.files[0]?.path ?? ''
    stackLogs()
  })

  function stackLogs() {
    panel = units.length ? { kind: 'logs', title: 'Logs', url: wsURL('/ws/logs', { unit: units }) } : undefined
  }

  async function run(label: string, fn: () => Promise<unknown>) {
    busy = label
    error = ''
    try {
      await fn()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = ''
      await refresh()
    }
  }

  const stackAction = (action: string) => run(action, () => api.stackAction(stack.id, action))
  const unitAction = (unit: string, action: string) => run(unit + action, () => api.unitAction(unit, action))

  function update() {
    panel = { kind: 'pull', title: 'Pulling images', url: wsURL('/ws/pull', { stack: stack.id }) }
  }

  async function afterPull(code: number) {
    if (panel?.kind !== 'pull' || code !== 0) return
    if (confirm('Images pulled. Restart the stack to use them?')) {
      await stackAction('restart')
      stackLogs()
    }
  }

  function containerName(unit: string) {
    const c = stack.containers.find((c) => c.Labels?.PODMAN_SYSTEMD_UNIT === unit)
    return c?.Names?.[0]
  }

  async function removeStack() {
    if (!confirm(`Remove the empty stack folder "${stack.name}"?`)) return
    await run('remove', () => api.removeStack(stack.id))
    if (!error) location.hash = '/'
  }
</script>

<section class="stack">
  <div class="head row wrap">
    <a class="back" href="#/">‹</a>
    <h1>{stack.name}</h1>
    <span class="badge"><span class={'dot ' + stack.state}></span>{stack.state}</span>
    {#if stack.id.startsWith('d/')}<span class="badge" title="Files live in this folder">folder</span>{/if}
    <span class="grow"></span>
    <button class="primary" disabled={!!busy || !units.length} onclick={() => stackAction('start')}>▶ Start</button>
    <button disabled={!!busy || !units.length} onclick={() => stackAction('restart')}>↻ Restart</button>
    <button disabled={!!busy || !units.length} onclick={() => stackAction('stop')}>■ Stop</button>
    <button disabled={!!busy || !stack.files.some((f) => f.image)} onclick={update} title="Pull newer images">⤓ Update</button>
    {#if stack.id.startsWith('d/') && stack.files.length === 0}
      <button class="danger" onclick={removeStack}>Remove</button>
    {/if}
  </div>
  {#if busy}<p class="muted">Working: {busy}…</p>{/if}
  {#if error}<div class="error-box">{error}</div>{/if}

  <div class="cols">
    <div class="left">
      <div class="files row wrap">
        {#each stack.files as f (f.path)}
          <button class="tab" class:active={f.path === file} onclick={() => (file = f.path)} title={f.path}>
            {#if f.unit}<span class={'dot ' + unitBadge(stack.units[f.unit]?.active)}></span>{/if}
            {f.path.slice(stack.dir ? stack.dir.length + 1 : 0)}
          </button>
        {/each}
        <button class="ghost" onclick={() => (newFileOpen = true)}>+ File</button>
      </div>
      {#if file}
        {#key file}
          <FileEditor
            path={file}
            file={stack.files.find((f) => f.path === file)}
            onchanged={refresh}
            onmoved={async (to: string) => {
              await refresh()
              file = to
            }}
            ondeleted={async () => {
              file = ''
              await refresh()
            }}
          />
        {/key}
      {:else}
        <p class="muted">This stack has no files yet. Add a container or pod with <b>+ File</b>.</p>
      {/if}
    </div>

    <div class="right">
      <div class="panel units">
        <table>
          <thead><tr><th>Unit</th><th>State</th><th>Container</th><th></th></tr></thead>
          <tbody>
            {#each stack.files.filter((f) => f.unit) as f (f.path)}
              {@const st = stack.units[f.unit!]}
              {@const cname = containerName(f.unit!)}
              <tr>
                <td>
                  <div class="mono">{f.unit}</div>
                  {#if f.image}<div class="muted small">{f.image}</div>{/if}
                </td>
                <td>
                  <span class="row"><span class={'dot ' + unitBadge(st?.active)}></span>{st?.load === 'not-found' ? 'not generated' : (st?.sub ?? 'unknown')}</span>
                  {#if f.autostart}<div class="muted small">starts at boot</div>{/if}
                </td>
                <td class="mono small">{cname ?? ''}</td>
                <td class="actions">
                  {#if st?.active === 'active'}
                    <button class="ghost" title="Restart" disabled={!!busy} onclick={() => unitAction(f.unit!, 'restart')}>↻</button>
                    <button class="ghost" title="Stop" disabled={!!busy} onclick={() => unitAction(f.unit!, 'stop')}>■</button>
                  {:else}
                    <button class="ghost" title="Start" disabled={!!busy} onclick={() => unitAction(f.unit!, 'start')}>▶</button>
                  {/if}
                  <button class="ghost" title="Logs" onclick={() => (panel = { kind: 'logs', title: 'Logs: ' + f.unit, url: wsURL('/ws/logs', { unit: f.unit! }) })}>☰</button>
                  {#if me?.terminal && cname && st?.active === 'active' && f.kind === 'container'}
                    <button class="ghost" title="Shell" onclick={() => (panel = { kind: 'exec', title: 'Shell: ' + cname, url: wsURL('/ws/exec', { container: cname }), interactive: true })}>⌘</button>
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="4" class="muted">No units</td></tr>
            {/each}
          </tbody>
        </table>
      </div>

      {#if wiring.length}
        <details class="panel wiring" open>
          <summary>Mounts, networks &amp; secrets</summary>
          {#each wiring as f (f.path)}
            <div class="unit-wiring">
              <div class="mono small"><b>{f.path.split('/').pop()}</b></div>
              {#each f.networks ?? [] as n}
                <div class="w"><span class="tag">net</span>{#if isResource(n)}<a href="#/resources" class="mono">{n}</a>{:else}<span class="mono">{n}</span>{/if}</div>
              {/each}
              {#each f.mounts ?? [] as m}
                <div class="w" title={m.options}>
                  <span class="tag">{m.kind === 'bind' ? 'bind' : m.kind === 'quadlet' ? 'vol' : m.kind}</span>
                  <span class="mono src">{#if m.kind === 'quadlet'}<a href="#/resources">{m.source}</a>{:else}{m.source ?? ''}{/if}</span>
                  <span class="muted">→</span>
                  <span class="mono">{m.target}</span>
                  {#if m.options}<span class="muted opts">{m.options}</span>{/if}
                </div>
              {/each}
              {#each f.secrets ?? [] as sec}
                {@const known = catalog.secrets.find((x) => x.name === sec.name)}
                <div class="w">
                  <span class="tag secret">secret</span>
                  <a href="#/secrets" class="mono">{sec.name}</a>
                  <span class="muted">→</span>
                  <span class="mono">{sec.type === 'env' ? '$' + sec.target : sec.target}</span>
                  {#if known && !known.exists}<span class="missing">missing</span>{/if}
                </div>
              {/each}
            </div>
          {/each}
        </details>
      {/if}

      {#if panel}
        <div class="term-head row">
          <b class="grow">{panel.title}</b>
          {#if panel.kind !== 'logs' || panel.url !== wsURL('/ws/logs', { unit: units })}
            <button class="ghost" onclick={stackLogs}>Stack logs</button>
          {/if}
          <button class="ghost" onclick={() => reconnect++}>Reconnect</button>
        </div>
        <div class="term-wrap">
          {#key reconnect}
            <Terminal url={panel.url} interactive={panel.interactive} onexit={afterPull} />
          {/key}
        </div>
      {/if}
    </div>
  </div>
</section>

{#if newFileOpen}
  <NewFileDialog
    {stack}
    onclose={() => (newFileOpen = false)}
    ondone={async (path: string) => {
      newFileOpen = false
      await refresh()
      file = path
    }}
  />
{/if}

<style>
  .stack {
    display: flex;
    flex-direction: column;
    gap: 12px;
    min-height: 100%;
  }
  h1 {
    margin: 0;
    font-size: 20px;
  }
  .back {
    display: none;
    font-size: 22px;
    color: var(--muted);
  }
  .cols {
    display: grid;
    grid-template-columns: minmax(0, 1.25fr) minmax(0, 1fr);
    gap: 14px;
    flex: 1;
    min-height: 0;
  }
  .left,
  .right {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 0;
  }
  .files {
    gap: 4px;
  }
  .tab {
    font-family: var(--mono);
    font-size: 12.5px;
  }
  .tab.active {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 12%, var(--panel));
  }
  .units {
    overflow-x: auto;
  }
  .small {
    font-size: 12px;
  }
  .actions {
    text-align: right;
    white-space: nowrap;
  }
  .actions button {
    padding: 3px 7px;
  }
  .wiring {
    padding: 8px 12px;
  }
  .wiring summary {
    cursor: pointer;
    font-weight: 500;
  }
  .unit-wiring {
    padding: 8px 0 4px;
    border-top: 1px solid var(--border);
    margin-top: 8px;
  }
  .unit-wiring:first-of-type {
    border-top: 0;
  }
  .w {
    display: flex;
    align-items: baseline;
    gap: 6px;
    font-size: 12.5px;
    min-width: 0;
  }
  .w .mono {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .src {
    max-width: 50%;
  }
  .opts {
    font-size: 11.5px;
  }
  .tag {
    flex: none;
    width: 44px;
    font-size: 11px;
    color: var(--muted);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .tag.secret {
    color: var(--warn);
  }
  .missing {
    color: var(--bad);
    font-size: 12px;
  }
  .term-head {
    margin-bottom: -4px;
  }
  .term-wrap {
    flex: 1;
    min-height: 320px;
  }
  @media (max-width: 1100px) {
    .cols {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 760px) {
    .back {
      display: inline;
    }
  }
</style>
