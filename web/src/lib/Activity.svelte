<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Stack } from './api'
  import { activity, isActive } from './operations.svelte'

  let { stacks, onchanged }: { stacks: Stack[]; onchanged: () => Promise<void> } = $props()
  let now = $state(Date.now())
  let expanded = $state(false)
  let checking = $state(false)
  let reloadError = $state('')
  const active = $derived(activity.items.filter(isActive).length)
  const shown = $derived(expanded ? activity.items : activity.items.slice(0, 5))
  const label = (state: string) => state.replaceAll('_', ' ')
  const badge = (state: string) => state === 'succeeded' ? 'running' : ['failed', 'timed_out', 'cancelled', 'unknown'].includes(state) ? 'failed' : ['running', 'queued'].includes(state) ? 'busy' : 'stopped'
  const stackFor = (unit: string) => stacks.find((stack) => stack.files.some((file) => file.unit === unit))

  function elapsed(start: string, end?: string) {
    const seconds = Math.max(0, Math.floor(((end ? Date.parse(end) : now) - Date.parse(start)) / 1000))
    return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  }

  async function reload() {
    reloadError = ''
    try {
      await api.reload()
      await onchanged()
    } catch (error) {
      reloadError = error instanceof Error ? error.message : String(error)
    }
  }

  onMount(() => {
    let stopped = false
    let poll: ReturnType<typeof setTimeout>
    const clock = setInterval(() => (now = Date.now()), 1000)
    async function load() {
      checking = true
      try {
        const previous = activity.items
        const known = new Set(previous.map((operation) => operation.id))
        const operations = await api.operations()
        if (stopped) return
        const added = activity.items.filter((operation) => !known.has(operation.id) && !operations.some((item) => item.id === operation.id))
        activity.items = [...added, ...operations.map((operation) => {
          const existing = activity.items.find((item) => item.id === operation.id)
          return existing && existing.revision > operation.revision ? existing : operation
        })]
        activity.error = ''
        if (operations.some((operation) => !isActive(operation) && previous.some((item) => item.id === operation.id && isActive(item)))) await onchanged()
      } catch (error) {
        if (!stopped) activity.error = `Cannot update action status. Actions may still be running: ${error instanceof Error ? error.message : error}`
      } finally {
        checking = false
        if (!stopped) poll = setTimeout(load, activity.items.some(isActive) ? 1000 : 5000)
      }
    }
    load()
    return () => {
      stopped = true
      clearTimeout(poll)
      clearInterval(clock)
    }
  })
</script>

<section class="panel activity" aria-label="Action activity">
  <div class="row wrap">
    <b class="grow">Activity {#if active}<span class="badge">{active} pending</span>{:else if activity.items[0]}<span class="muted latest">Latest: {label(activity.items[0].state)}</span>{/if}</b>
    {#if checking}<span class="muted small">Checking…</span>{/if}
    <button class="ghost" disabled={active > 0} onclick={reload}>Reload systemd</button>
    {#if activity.items.length}<button class="ghost" onclick={() => (expanded = !expanded)}>{expanded ? 'Show recent' : 'Show all'}</button>{/if}
  </div>
  {#if activity.error}<div class="error-box" role="alert">{activity.error}</div>{/if}
  {#if activity.warning}<div class="error-box" role="alert">{activity.warning}<button class="ghost" onclick={() => (activity.warning = '')}>Dismiss</button></div>{/if}
  {#if reloadError}<div class="error-box" role="alert">{reloadError}</div>{/if}
  {#each shown as operation (operation.id)}
    <details open={isActive(operation) || ['failed', 'timed_out', 'cancelled', 'unknown'].includes(operation.state)}>
      <summary>
        <span class={'dot ' + badge(operation.state)}></span>
        <span class="grow">{operation.label}</span>
        <span class="state">{label(operation.state)}</span>
        <span class="muted duration">{elapsed(operation.started ?? operation.created, operation.finished)}</span>
      </summary>
      {#if operation.state === 'queued'}<p class="muted small">Waiting for another action. No command has been sent yet.</p>{/if}
      {#if operation.error && !operation.steps.some((step) => step.error)}<div class="error-box">{operation.error}</div>{/if}
      {#if isActive(operation)}<p class="muted small">Deadline: {new Date(operation.deadline).toLocaleTimeString()}. Navigating or closing this page does not cancel the action.</p>{/if}
      {#if operation.started && ['timed_out', 'cancelled', 'unknown'].includes(operation.state)}<p class="error-box">Observation ended. An already-submitted systemd job or container command may still be running; check its state and logs before retrying.</p>{/if}
      {#each operation.steps as step}
        <div class="step">
          <div class="row wrap small">
            <span class={'dot ' + badge(step.state)}></span>
            <code class="grow">{step.name}</code>
            <span>{label(step.state)}</span>
            {#if step.started}<span class="muted duration">{elapsed(step.started, step.finished)}</span>{/if}
            {#if step.unit && stackFor(step.unit)}<a href={'#/stack/' + encodeURIComponent(stackFor(step.unit)!.id)}>Stack &amp; logs</a>{/if}
          </div>
          {#if step.error}<div class="error-box">{step.error}</div>{/if}
          {#if step.detail}<pre>{step.detail}</pre>{/if}
        </div>
      {/each}
    </details>
  {:else}
    <p class="muted small">Actions and their results appear here, including after a page reload.</p>
  {/each}
</section>

<style>
  .activity {
    padding: 10px 12px;
  }
  .small,
  .latest {
    font-size: 12px;
    font-weight: normal;
  }
  .latest {
    margin-left: 8px;
  }
  details {
    margin-top: 8px;
    border-top: 1px solid var(--border);
    padding-top: 8px;
  }
  summary {
    display: flex;
    align-items: center;
    gap: 8px;
    cursor: pointer;
  }
  .state {
    font-size: 12px;
    text-transform: capitalize;
  }
  .duration {
    min-width: 40px;
    text-align: right;
    font-size: 12px;
    font-variant-numeric: tabular-nums;
  }
  .step {
    padding: 8px 0 0 18px;
  }
  pre {
    max-height: 180px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-size: 12px;
  }
  p {
    margin: 8px 0 0;
  }
</style>
