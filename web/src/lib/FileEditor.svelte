<script lang="ts">
  import { onMount } from 'svelte'
  import { api, ApiError, type Apply, type QFile, type Validation, type Version } from './api'
  import Editor from './Editor.svelte'
  import HistoryDialog from './HistoryDialog.svelte'
  import SecretHelperDialog from './SecretHelperDialog.svelte'
  import { activity, isActive } from './operations.svelte'

  let {
    path,
    file,
    onchanged,
    ondeleted,
    onmoved,
  }: { path: string; file?: QFile; onchanged: () => Promise<void>; ondeleted: () => void; onmoved: (to: string) => void } = $props()

  let content = $state('')
  let saved = $state('')
  let loading = $state(true)
  let busy = $state(false)
  let working = $state('')
  let error = $state('')
  let notice = $state('')
  let validation = $state<Validation>()
  let showGenerated = $state(false)
  let historyOpen = $state(false)
  let secretsOpen = $state(false)

  const dirty = $derived(content !== saved)
  const applying = $derived(activity.items.some((operation) => isActive(operation) && operation.label === 'Apply ' + path))

  onMount(async () => {
    try {
      const r = await api.readFile(path)
      content = saved = r.content
    } catch (e: any) {
      error = e.message
    } finally {
      loading = false
    }
  })

  async function validate() {
    if (busy || applying) return
    busy = true
    working = 'Checking with the Quadlet generator…'
    error = notice = ''
    try {
      validation = await api.validate(path, content)
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  async function save(apply: Apply, force = false) {
    if ((busy && !force) || applying || loading) return
    if (apply === 'restart' && !force && file?.sharedBy?.length && !confirm(`Sharing the network of ${file.unit}: ${file.sharedBy.join(', ')}.\n\nThose that are running are stopped first and started again afterwards. Continue?`)) return
    busy = true
    working = 'Saving' + (apply ? ' and queuing ' + apply : '') + '…'
    error = notice = ''
    const submitted = content
    try {
      const r = await api.saveFile(path, submitted, apply, force)
      saved = submitted
      validation = r.validation
      notice = r.applyError
        ? ''
        : r.operation ? 'Saved. Applying is queued; see Activity for progress and the result.' : 'Saved. Reload systemd to apply.'
      if (r.applyError) error = r.applyError
      await onchanged()
    } catch (e: any) {
      if (e instanceof ApiError && e.status === 422) {
        validation = e.body.validation
        if (confirm('Podman rejected this file:\n\n' + validation!.output + '\n\nSave anyway?')) return save(apply, true)
      } else {
        error = e.message
      }
    } finally {
      busy = false
    }
  }

  async function remove() {
    if (busy || applying) return
    const stop = !!file?.unit && confirm(`Stop ${file.unit} before deleting? (Cancel deletes without stopping.)`)
    if (!confirm(`Delete ${path}? A copy stays in the history.`)) return
    busy = true
    working = 'Deleting (see Activity)…'
    try {
      await api.deleteFile(path, stop)
      ondeleted()
    } catch (e: any) {
      error = e.message
      if (e.body?.operation?.steps.some((step: { name: string; state: string }) => step.name === 'Delete ' + path && step.state === 'succeeded')) ondeleted()
    } finally {
      busy = false
    }
  }

  async function move() {
    if (busy || applying) return
    const to = prompt('Move or rename to (relative to the Quadlet directory):', path)?.trim()
    if (!to || to === path) return
    busy = true
    working = 'Moving…'
    try {
      const result = await api.moveFile(path, to)
      if (result.applyError) activity.warning = result.applyError
      onmoved(to)
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  function restore(v: Version, text: string) {
    content = text
    historyOpen = false
    notice = `Loaded the version from ${new Date(v.time).toLocaleString()}. Save to keep it.`
  }

  const problems = $derived(validation && !validation.ok ? validation.output : '')
</script>

<div class="fe">
  <div class="bar row wrap">
    <code class="grow path">{path}{dirty ? ' •' : ''}</code>
    <button disabled={busy || loading} onclick={validate}>Check</button>
    <button disabled={busy || loading} onclick={() => (historyOpen = true)}>History</button>
    {#if file?.kind === 'container'}
      <button disabled={busy || loading} onclick={() => (secretsOpen = true)} title="Move values into Podman secrets">Secrets</button>
    {/if}
    <button class="ghost" disabled={busy || applying} onclick={move} title="Move or rename">⇄</button>
    <button class="ghost danger" disabled={busy || applying} onclick={remove} title="Delete">🗑</button>
  </div>

  {#if loading}
    <p class="muted">Loading…</p>
  {:else}
    <div class="ed">
      <Editor bind:value={content} {problems} onsave={() => save('reload')} />
    </div>
  {/if}

  <div class="bar row wrap">
    <button disabled={busy || applying || !dirty} onclick={() => save('')}>Save</button>
    <button disabled={busy || applying || !dirty} onclick={() => save('reload')} title="Save and daemon-reload (⌘S)">Save &amp; reload</button>
    {#if file?.unit && ['container', 'pod', 'kube'].includes(file.kind)}
      <button class="primary" disabled={busy || applying} onclick={() => save('restart')} title="Save, daemon-reload and restart {file.unit}">Deploy</button>
    {/if}
    <span class="grow"></span>
    {#if validation?.generated}
      <button class="ghost" onclick={() => (showGenerated = !showGenerated)}>{showGenerated ? 'Hide' : 'Show'} generated unit</button>
    {/if}
  </div>

  {#if busy}<p class="muted" role="status">{working}</p>{/if}
  {#if error}<div class="error-box">{error}</div>{/if}
  {#if validation && !validation.ok}
    <div class="error-box">{validation.output}</div>
  {:else if validation?.skipped}
    <div class="error-box">{validation.skipped}</div>
  {:else if notice || validation}
    <div class="ok-box">
      {notice || 'Podman accepts this file.'}
      {#if validation?.output}<pre class="warn">{validation.output}</pre>{/if}
    </div>
  {/if}
  {#if showGenerated && validation?.generated}
    <pre class="generated panel">{validation.generated}</pre>
  {/if}
</div>

{#if secretsOpen}
  <SecretHelperDialog
    {path}
    {content}
    onclose={() => (secretsOpen = false)}
    onapply={(next: string, msg: string) => {
      content = next
      secretsOpen = false
      notice = msg
    }}
  />
{/if}

{#if historyOpen}
  <HistoryDialog {path} current={content} onclose={() => (historyOpen = false)} onrestore={restore} />
{/if}

<style>
  .fe {
    display: flex;
    flex-direction: column;
    gap: 8px;
    flex: 1;
    min-height: 0;
  }
  .path {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .ed {
    flex: 1;
    min-height: 360px;
    display: flex;
  }
  .ed > :global(*) {
    flex: 1;
  }
  .generated {
    margin: 0;
    padding: 10px 12px;
    overflow: auto;
    max-height: 320px;
    font-family: var(--mono);
    font-size: 12px;
  }
  .warn {
    margin: 6px 0 0;
    white-space: pre-wrap;
    font-family: var(--mono);
    font-size: 12px;
    color: var(--warn);
  }
</style>
