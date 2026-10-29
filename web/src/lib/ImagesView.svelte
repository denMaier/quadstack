<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Image } from './api'

  let images = $state<Image[]>([])
  let error = $state('')
  let output = $state('')
  let busy = $state(false)

  const total = $derived(images.reduce((n, i) => n + i.Size, 0))

  async function load() {
    try {
      images = (await api.images()).sort((a, b) => b.Created - a.Created)
    } catch (e: any) {
      error = e.message
    }
  }

  async function prune(all: boolean) {
    const what = all ? 'every image not used by a container' : 'dangling images'
    if (!confirm(`Remove ${what}?`)) return
    busy = true
    try {
      output = (await api.prune(all)).output || 'Nothing to remove.'
      await load()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  function size(n: number) {
    const u = ['B', 'KB', 'MB', 'GB', 'TB']
    let i = 0
    while (n >= 1000 && i < u.length - 1) {
      n /= 1000
      i++
    }
    return `${n.toFixed(i ? 1 : 0)} ${u[i]}`
  }

  onMount(load)
</script>

<section>
  <div class="row wrap head">
    <h1 class="grow">Images</h1>
    <span class="muted">{images.length} images · {size(total)}</span>
    <button disabled={busy} onclick={() => prune(false)}>Prune dangling</button>
    <button class="danger" disabled={busy} onclick={() => prune(true)}>Prune unused</button>
  </div>
  {#if error}<div class="error-box">{error}</div>{/if}
  {#if output}<pre class="panel out">{output}</pre>{/if}
  <div class="panel scroll">
    <table>
      <thead><tr><th>Image</th><th>ID</th><th>Size</th><th>Created</th><th>In use</th></tr></thead>
      <tbody>
        {#each images as img (img.Id)}
          <tr>
            <td class="mono">{img.Names?.join(', ') ?? '<none>'}</td>
            <td class="mono muted">{img.Id.slice(0, 12)}</td>
            <td>{size(img.Size)}</td>
            <td>{new Date(img.Created * 1000).toLocaleDateString()}</td>
            <td>{img.Containers || ''}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
</section>

<style>
  h1 {
    margin: 0;
    font-size: 20px;
  }
  .head {
    margin-bottom: 12px;
  }
  .scroll {
    overflow-x: auto;
  }
  .out {
    margin: 0 0 12px;
    padding: 10px 12px;
    font-family: var(--mono);
    font-size: 12px;
    white-space: pre-wrap;
  }
</style>
