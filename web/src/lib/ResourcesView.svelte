<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type PodmanNetwork, type PodmanVolume, type Resource, type Stack } from './api'
  import { catalog } from './catalog.svelte'
  import { template, unitBadge } from './quadlet'
  import FileEditor from './FileEditor.svelte'

  let { stacks, refresh }: { stacks: Stack[]; refresh: () => Promise<void> } = $props()

  let networks = $state<PodmanNetwork[]>([])
  let volumes = $state<PodmanVolume[]>([])
  let error = $state('')
  let open = $state('')

  const stackName = (id: string) => stacks.find((s) => s.id === id)?.name ?? id
  const defined = (kind: string) => catalog.resources.filter((r) => r.file.kind === kind)
  const extras = (r: Resource) => catalog.resources.filter((x) => x.file.kind === 'dropin' && x.file.path.startsWith(r.file.path + '.d/'))
  // Podman networks and volumes no Quadlet file defines
  const otherNetworks = $derived(networks.filter((n) => !catalog.resources.some((r) => r.file.kind === 'network' && r.file.name === n.name)))
  const otherVolumes = $derived(volumes.filter((v) => !catalog.resources.some((r) => r.file.kind === 'volume' && r.file.name === v.Name)))

  async function load() {
    try {
      const r = await api.podmanResources()
      networks = r.networks ?? []
      volumes = r.volumes ?? []
    } catch (e: any) {
      error = e.message
    }
  }

  async function create(kind: 'network' | 'volume') {
    const name = prompt(`Name of the new shared ${kind}:`)?.trim()
    if (!name) return
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name)) {
      alert('Use letters, digits, dot, dash or underscore.')
      return
    }
    try {
      await api.createFile(`${name}.${kind}`, template(kind, name))
      await refresh()
      open = `${name}.${kind}`
    } catch (e: any) {
      error = e.body?.validation?.output || e.message
    }
  }

  onMount(load)
</script>

<section>
  <h1>Networks &amp; Volumes</h1>
  <p class="muted">
    Network and volume files at the top of the Quadlet directory are shared: any stack can use them with
    <code>Network=name.network</code> or <code>Volume=name.volume:/path</code>, and systemd creates them when a unit
    that needs them starts. Files inside a stack folder belong to that stack.
  </p>
  {#if error}<div class="error-box">{error}</div>{/if}

  {#each [['network', 'Networks'], ['volume', 'Volumes']] as [kind, title] (kind)}
    <div class="row head">
      <h2 class="grow">{title}</h2>
      <button onclick={() => create(kind as 'network' | 'volume')}>+ New {kind}</button>
    </div>
    <div class="panel scroll">
      <table>
        <thead><tr><th>Podman name</th><th>File</th><th>Used by</th><th>State</th></tr></thead>
        <tbody>
          {#each defined(kind) as r (r.file.path)}
            <tr class="click" class:open={open === r.file.path} onclick={() => (open = open === r.file.path ? '' : r.file.path)}>
              <td><code>{r.file.name}</code></td>
              <td>
                <code class="muted">{r.file.path}</code>
                {#if r.stack}<span class="badge">in {stackName(r.stack)}</span>{/if}
                {#each extras(r) as d}<div class="small muted">+ {d.file.path}</div>{/each}
              </td>
              <td>
                {#each r.usedBy as id}
                  <a class="chip" href={'#/stack/' + encodeURIComponent(id)} onclick={(e) => e.stopPropagation()}>{stackName(id)}</a>
                {:else}
                  <span class="muted small">unused</span>
                {/each}
              </td>
              <td>
                {#if kind === 'network' ? networks.some((n) => n.name === r.file.name) : volumes.some((v) => v.Name === r.file.name)}
                  <span class="row small"><span class="dot running"></span>created</span>
                {:else}
                  <span class="row small"><span class={'dot ' + unitBadge(r.state?.active)}></span>not created yet</span>
                {/if}
              </td>
            </tr>
            {#if open === r.file.path}
              <tr class="editor-row">
                <td colspan="4">
                  <FileEditor
                    path={r.file.path}
                    file={r.file}
                    onchanged={refresh}
                    ondeleted={async () => {
                      open = ''
                      await refresh()
                    }}
                    onmoved={async (to: string) => {
                      open = to
                      await refresh()
                    }}
                  />
                </td>
              </tr>
            {/if}
          {:else}
            <tr><td colspan="4" class="muted">No {kind} files.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>

    {@const others = kind === 'network' ? otherNetworks.map((n) => ({ name: n.name, info: [n.driver, n.subnets?.map((s) => s.subnet).join(', '), n.internal ? 'internal' : ''].filter(Boolean).join(' · ') })) : otherVolumes.map((v) => ({ name: v.Name, info: v.Driver }))}
    {#if others.length}
      <p class="muted small other-title">Also in Podman, not defined by a Quadlet file:</p>
      <div class="others">
        {#each others as o}<span class="badge"><code>{o.name}</code>{#if o.info}&nbsp;{o.info}{/if}</span>{/each}
      </div>
    {/if}
  {/each}
</section>

<style>
  section {
    max-width: 1100px;
  }
  h1 {
    margin: 0 0 8px;
    font-size: 20px;
  }
  h2 {
    margin: 0;
    font-size: 16px;
  }
  .head {
    margin: 22px 0 8px;
  }
  .scroll {
    overflow-x: auto;
  }
  .click {
    cursor: pointer;
  }
  .click:hover,
  .open {
    background: var(--panel-2);
  }
  .editor-row td {
    background: var(--panel-2);
  }
  .editor-row :global(.fe) {
    min-height: 420px;
  }
  .chip {
    display: inline-block;
    margin: 0 4px 2px 0;
    padding: 1px 8px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent) 14%, transparent);
    font-size: 12.5px;
  }
  .small {
    font-size: 12.5px;
  }
  .other-title {
    margin: 10px 0 6px;
  }
  .others {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
</style>
