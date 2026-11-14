<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type AutoUpdateTimer } from './api'
  import { activity, isActive } from './operations.svelte'

  let timer = $state<AutoUpdateTimer>()
  let readError = $state('')
  let actionError = $state('')
  let busy = $state(false)
  let loading = false

  const pending = $derived(busy || activity.items.some((op) => isActive(op) && op.steps.some((step) => step.unit === 'podman-auto-update.timer')))
  const on = $derived(!!timer && (timer.enabled || timer.active === 'active' || timer.active === 'activating'))
  const label = $derived.by(() => {
    if (readError || !timer) return 'Unknown'
    if (timer.load !== 'loaded') return timer.load === 'masked' ? 'Masked' : 'Unavailable'
    if (timer.active === 'failed') return 'Failed'
    if (timer.active === 'activating' || timer.active === 'deactivating') return 'Changing'
    if (timer.active === 'active') {
      if (timer.unitFileState === 'enabled') return 'On'
      return timer.enabled ? 'On until logout' : 'Running, not enabled at login'
    }
    return timer.enabled ? 'Enabled, stopped' : 'Off'
  })
  const date = (value: string) => new Date(value).toLocaleString()

  async function refresh() {
    if (loading) return
    loading = true
    try {
      timer = await api.autoUpdate()
      readError = ''
    } catch (error: any) {
      timer = undefined
      readError = error.message
    } finally {
      loading = false
    }
  }

  async function change(enabled: boolean) {
    busy = true
    actionError = ''
    try {
      await api.setAutoUpdate(enabled)
    } catch (error: any) {
      actionError = error.message
    } finally {
      await refresh()
      busy = false
    }
  }

  onMount(() => {
    refresh()
    const interval = setInterval(() => {
      if (!busy && document.visibilityState === 'visible') refresh()
    }, 5000)
    return () => clearInterval(interval)
  })
</script>

<section class="panel updates" aria-label="Scheduled updates">
  <div class="row wrap">
    <label class="row control">
      <input type="checkbox" role="switch" checked={on} disabled={pending || !!readError || timer?.load !== 'loaded'} onchange={(event) => {
        const enabled = event.currentTarget.checked
        event.currentTarget.checked = on
        change(enabled)
      }} />
      <b>Scheduled updates</b>
    </label>
    <span class="badge">{pending ? 'Changing…' : label}</span>
  </div>
  <p class="muted">Podman updates and restarts this account's running containers that opt in, including containers outside Quadstack. Enabling may run an overdue update immediately.</p>
  {#if timer?.load === 'loaded'}
    <dl>
      <div><dt>Schedule</dt><dd>{timer.schedule.join(', ') || 'Custom timer'}{#if timer.randomizedDelay} <span class="muted">(+ up to {Math.ceil(timer.randomizedDelay / 60)} min delay)</span>{/if}</dd></div>
      <div><dt>Next run</dt><dd>{timer.nextRun ? date(timer.nextRun) : timer.active === 'active' ? 'Unavailable' : 'Not scheduled'}</dd></div>
      <div><dt>Last run</dt><dd>{#if timer.running}Running…{:else if timer.lastRun}{date(timer.lastRun)} · {timer.lastResult || 'Unknown result'}{:else}No result available{/if}</dd></div>
    </dl>
  {:else if timer}
    <p class="muted">Podman's user timer is {timer.load || 'unavailable'}. Check that the Podman systemd user units are installed and the timer is not masked.</p>
  {/if}
  <p class="muted small">Participation appears on each stack. Change <code>AutoUpdate=</code> in its Quadlet files and deploy to change it. Turning this off stops future timer runs; an update already running continues.</p>
  {#if readError}<div class="error-box">{readError}</div>{/if}
  {#if actionError}<div class="error-box">{actionError}</div>{/if}
</section>

<style>
  .updates { padding: 14px; margin-bottom: 16px; }
  .control { cursor: pointer; }
  p { margin: 10px 0; }
  .small { font-size: 12px; margin-bottom: 0; }
  dl { margin: 12px 0; font-size: 13px; }
  dl div { display: flex; gap: 12px; margin-top: 5px; }
  dt { color: var(--muted); flex: 0 0 76px; }
  dd { margin: 0; overflow-wrap: anywhere; }
</style>
