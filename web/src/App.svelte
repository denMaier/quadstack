<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Me, type Stack } from './lib/api'
  import StackView from './lib/StackView.svelte'
  import ImagesView from './lib/ImagesView.svelte'
  import ResourcesView from './lib/ResourcesView.svelte'
  import SecretsView from './lib/SecretsView.svelte'
  import { catalog, loadSecrets } from './lib/catalog.svelte'
  import ImportDialog from './lib/ImportDialog.svelte'

  let me = $state<Me>()
  let stacks = $state<Stack[]>([])
  let warnings = $state<string[]>([])
  let loadError = $state('')
  let filter = $state('')
  let route = $state(location.hash.slice(1) || '/')
  let importOpen = $state(false)

  const stackID = $derived(route.startsWith('/stack/') ? decodeURIComponent(route.slice(7)) : '')
  const current = $derived(stacks.find((s) => s.id === stackID))
  const shown = $derived(stacks.filter((s) => s.name.toLowerCase().includes(filter.toLowerCase())))
  const counts = $derived({
    running: stacks.filter((s) => s.state === 'running').length,
    partial: stacks.filter((s) => s.state === 'partial').length,
    failed: stacks.filter((s) => s.state === 'failed').length,
    stopped: stacks.filter((s) => s.state === 'stopped').length,
  })

  function go(path: string) {
    location.hash = path
  }

  async function refresh() {
    try {
      const r = await api.stacks()
      stacks = r.stacks
      catalog.resources = r.resources ?? []
      warnings = r.warnings ?? []
      loadError = ''
    } catch (e: any) {
      loadError = e.message
    }
  }

  async function newStack() {
    const name = prompt('Name of the new stack (a folder in the Quadlet directory):')?.trim()
    if (!name) return
    try {
      const { id } = await api.createStack(name)
      await refresh()
      go('/stack/' + encodeURIComponent(id))
    } catch (e: any) {
      alert(e.message)
    }
  }

  onMount(() => {
    api.me().then((m) => (me = m))
    refresh()
    loadSecrets()
    const timer = setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
    const onHash = () => (route = location.hash.slice(1) || '/')
    addEventListener('hashchange', onHash)
    return () => {
      clearInterval(timer)
      removeEventListener('hashchange', onHash)
    }
  })
</script>

<div class="shell">
  <header>
    <a class="brand" href="#/">
      <svg viewBox="0 0 32 32" width="20" height="20" aria-hidden="true">
        <rect x="3" y="3" width="11" height="11" rx="2" fill="var(--accent)" />
        <rect x="18" y="3" width="11" height="11" rx="2" fill="var(--accent)" opacity=".7" />
        <rect x="3" y="18" width="11" height="11" rx="2" fill="var(--accent)" opacity=".7" />
        <rect x="18" y="18" width="11" height="11" rx="2" fill="var(--accent)" opacity=".45" />
      </svg>
      Quadstack
    </a>
    {#if me}<span class="muted host">{me.host}</span>{/if}
    <span class="grow"></span>
    <nav class="top">
      <a href="#/resources" class:active={route === '/resources'}>Networks &amp; volumes</a>
      <a href="#/secrets" class:active={route === '/secrets'}>Secrets</a>
      <a href="#/images" class:active={route === '/images'}>Images</a>
    </nav>
    {#if me}<span class="muted">{me.name}</span>{/if}
    <a href="/auth/logout">Sign out</a>
  </header>

  <aside class:hidden-mobile={route !== '/'}>
    <div class="row">
      <button class="primary grow" onclick={newStack}>+ New stack</button>
      {#if me?.podlet}<button onclick={() => (importOpen = true)} title="Convert docker run or compose">Import</button>{/if}
    </div>
    <input type="search" placeholder="Search stacks" bind:value={filter} />
    <nav>
      {#each shown as s (s.id)}
        <a href={'#/stack/' + encodeURIComponent(s.id)} class:active={s.id === stackID}>
          <span class={'dot ' + s.state}></span>
          <span class="grow name">{s.name}</span>
          {#if s.id.startsWith('d/')}<span class="muted folder" title="Stack folder">▸</span>{/if}
          <span class="muted count">{s.files.filter((f) => f.unit).length}</span>
        </a>
      {:else}
        <p class="muted empty">{stacks.length ? 'No matches' : 'No Quadlet files yet'}</p>
      {/each}
    </nav>
  </aside>

  <main>
    {#if loadError}<div class="error-box">{loadError}</div>{/if}
    {#if me?.journal}<div class="error-box">{me.journal}</div>{/if}
    {#each warnings as w}<div class="error-box">{w}</div>{/each}

    {#if route === '/images'}
      <ImagesView />
    {:else if route === '/resources'}
      <ResourcesView {stacks} {refresh} />
    {:else if route === '/secrets'}
      <SecretsView />
    {:else if stackID}
      {#if current}
        <StackView stack={current} {me} {refresh} />
      {:else if stacks.length}
        <p class="muted">This stack no longer exists. <a href="#/">Back</a></p>
      {/if}
    {:else}
      <section class="overview">
        <h1>Stacks</h1>
        <div class="stats">
          <div class="panel"><span class="dot running"></span><b>{counts.running}</b> running</div>
          <div class="panel"><span class="dot partial"></span><b>{counts.partial}</b> partly running</div>
          <div class="panel"><span class="dot failed"></span><b>{counts.failed}</b> failed</div>
          <div class="panel"><span class="dot"></span><b>{counts.stopped}</b> stopped</div>
        </div>
        {#if me}
          <p class="muted">Managing <code>{me.quadletDir}</code> on <b>{me.host}</b>.
            {#if !me.validation}<br />Podman's quadlet generator was not found, so files are saved without validation.{/if}</p>
        {/if}
        <p class="muted">
          A stack is a folder in the Quadlet directory, or a top-level <code>.pod</code> together with the containers
          that set <code>Pod=</code> to it. Every other top-level unit is a stack of its own. Top-level networks and
          volumes are shared; see <a href="#/resources">Networks &amp; volumes</a>.
        </p>
      </section>
    {/if}
  </main>
</div>

{#if importOpen}
  <ImportDialog
    {stacks}
    onclose={() => (importOpen = false)}
    ondone={async (id: string) => {
      importOpen = false
      await refresh()
      go('/stack/' + encodeURIComponent(id))
    }}
  />
{/if}

<style>
  .shell {
    display: grid;
    grid-template-columns: 260px 1fr;
    grid-template-rows: 48px 1fr;
    height: 100%;
  }
  header {
    grid-column: 1 / -1;
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 0 16px;
    background: var(--panel);
    border-bottom: 1px solid var(--border);
  }
  header a {
    color: var(--text);
  }
  header a.active {
    color: var(--accent);
  }
  .top {
    display: flex;
    gap: 16px;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 8px;
    font-weight: 600;
  }
  aside {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-height: 0;
    padding: 12px;
    border-right: 1px solid var(--border);
    background: var(--panel);
  }
  nav {
    flex: 1;
    overflow-y: auto;
    margin: 0 -6px;
  }
  nav a {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 7px 8px;
    border-radius: 6px;
    color: var(--text);
  }
  nav a:hover {
    background: var(--panel-2);
  }
  nav a.active {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .count,
  .folder {
    font-size: 12px;
  }
  .empty {
    padding: 8px;
  }
  main {
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-width: 0;
    min-height: 0;
    padding: 16px;
    overflow: auto;
  }
  h1 {
    margin: 0 0 12px;
    font-size: 20px;
  }
  .overview {
    max-width: 760px;
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 10px;
    margin-bottom: 16px;
  }
  .stats .panel {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 14px;
  }
  .stats b {
    font-size: 20px;
  }
  @media (max-width: 760px) {
    .shell {
      grid-template-columns: 1fr;
      grid-template-rows: 48px auto 1fr;
    }
    .host {
      display: none;
    }
    header {
      gap: 10px;
      overflow-x: auto;
      white-space: nowrap;
    }
    .top {
      gap: 10px;
    }
    aside {
      border-right: 0;
      border-bottom: 1px solid var(--border);
      max-height: 45vh;
    }
    aside.hidden-mobile {
      display: none;
    }
  }
</style>
