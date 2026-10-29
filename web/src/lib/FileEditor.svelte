<script lang="ts">
  import { onMount } from 'svelte'
  import { api, ApiError, type Apply, type Stack, type Validation, type Version } from './api'
  import Editor from './Editor.svelte'
  import HistoryDialog from './HistoryDialog.svelte'

  let {
    path,
    stack,
    onchanged,
    ondeleted,
    onmoved,
  }: { path: string; stack: Stack; onchanged: () => Promise<void>; ondeleted: () => void; onmoved: (to: string) => void } = $props()

  let content = $state('')
  let saved = $state('')
  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')
  let notice = $state('')
  let validation = $state<Validation>()
  let showGenerated = $state(false)
  let historyOpen = $state(false)

  const dirty = $derived(content !== saved)
  const file = $derived(stack.files.find((f) => f.path === path))

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
    busy = true
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
    busy = true
    error = notice = ''
    try {
      const r = await api.saveFile(path, content, apply, force)
      saved = content
      validation = r.validation
      notice = r.applyError
        ? ''
        : { '': 'Saved. Reload systemd to apply.', reload: 'Saved and reloaded.', restart: 'Saved, reloaded and restarted.' }[apply]
      if (r.applyError) error = 'Saved, but applying failed: ' + r.applyError
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
    const stop = !!file?.unit && confirm(`Stop ${file.unit} before deleting? (Cancel deletes without stopping.)`)
    if (!confirm(`Delete ${path}? A copy stays in the history.`)) return
    busy = true
    try {
      await api.deleteFile(path, stop)
      ondeleted()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  async function move() {
    const to = prompt('Move or rename to (relative to the Quadlet directory):', path)?.trim()
    if (!to || to === path) return
    try {
      await api.moveFile(path, to)
      onmoved(to)
    } catch (e: any) {
      error = e.message
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
    <button class="ghost" disabled={busy} onclick={move} title="Move or rename">⇄</button>
    <button class="ghost danger" disabled={busy} onclick={remove} title="Delete">🗑</button>
  </div>

  {#if loading}
    <p class="muted">Loading…</p>
  {:else}
    <div class="ed">
      <Editor bind:value={content} {problems} onsave={() => save('reload')} />
    </div>
  {/if}

  <div class="bar row wrap">
    <button disabled={busy || !dirty} onclick={() => save('')}>Save</button>
    <button disabled={busy || !dirty} onclick={() => save('reload')} title="Save and daemon-reload (⌘S)">Save &amp; reload</button>
    {#if file?.unit && ['container', 'pod', 'kube'].includes(file.kind)}
      <button class="primary" disabled={busy} onclick={() => save('restart')} title="Save, daemon-reload and restart {file.unit}">Deploy</button>
    {/if}
    <span class="grow"></span>
    {#if validation?.generated}
      <button class="ghost" onclick={() => (showGenerated = !showGenerated)}>{showGenerated ? 'Hide' : 'Show'} generated unit</button>
    {/if}
  </div>

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
