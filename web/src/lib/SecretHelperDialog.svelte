<script lang="ts">
  import { onMount, untrack } from 'svelte'
  import { api } from './api'
  import { catalog, loadSecrets } from './catalog.svelte'

  // Rewrites a .container file so values come from Podman secrets. Secrets are created
  // right away; the file change is handed back to the editor and still has to be saved.
  let {
    path,
    content,
    onclose,
    onapply,
  }: { path: string; content: string; onclose: () => void; onapply: (content: string, notice: string) => void } = $props()

  let dialog: HTMLDialogElement
  let error = $state('')
  let busy = $state(false)
  // A snapshot of the file when the dialog opened
  const stem = untrack(() => path).split('/').pop()!.replace(/\.\w+$/, '')
  const lines = untrack(() => content).split('\n')

  // Environment=KEY=value lines with a single assignment
  const envs = lines
    .map((text, i) => {
      const m = text.match(/^\s*Environment=\s*"?([A-Za-z_][A-Za-z0-9_]*)=(.*?)"?\s*$/)
      return m && !m[2].includes(' ') ? { i, key: m[1], value: m[2] } : null
    })
    .filter((x) => x !== null)

  // Volume=source:target[:options] bind mounts, candidates for file secrets
  const binds = lines
    .map((text, i) => {
      const m = text.match(/^\s*Volume=\s*((?:\/|%h)[^:]*):([^:]+)(?::(.*))?$/)
      return m ? { i, source: m[1], target: m[2], options: m[3] ?? '' } : null
    })
    .filter((x) => x !== null)

  let name = $state<Record<string, string>>({})
  let existing = $state('')
  let refType = $state<'env' | 'mount'>('env')
  let refTarget = $state('')

  onMount(() => {
    loadSecrets()
    dialog.showModal()
  })

  const secretName = (s: string) =>
    s.toLowerCase().replace(/[^a-z0-9_.-]+/g, '-').replace(/^-+|-+$/g, '')

  function mask(v: string) {
    return v.length <= 4 ? '••••' : v.slice(0, 2) + '•'.repeat(Math.min(8, v.length - 2))
  }

  // Inserts a line at the end of the [Container] section
  function insert(ls: string[], line: string) {
    const start = ls.findIndex((l) => /^\s*\[Container\]/.test(l))
    if (start < 0) return [...ls, '', '[Container]', line]
    let end = start + 1
    while (end < ls.length && !/^\s*\[/.test(ls[end])) end++
    while (end > start + 1 && ls[end - 1].trim() === '') end--
    return [...ls.slice(0, end), line, ...ls.slice(end)]
  }

  async function exists(n: string) {
    await loadSecrets()
    return catalog.secrets.some((s) => s.name === n && s.exists)
  }

  async function run(fn: () => Promise<void>) {
    busy = true
    error = ''
    try {
      await fn()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  const moveEnv = (e: (typeof envs)[number]) =>
    run(async () => {
      const n = name['e' + e.i] || secretName(`${stem}-${e.key}`)
      if ((await exists(n)) && !confirm(`Replace the value of the existing secret "${n}"?`)) return
      await api.setSecret({ name: n, value: e.value, replace: true })
      const ls = [...lines]
      ls[e.i] = `Secret=${n},type=env,target=${e.key}`
      onapply(ls.join('\n'), `Created secret "${n}" and replaced Environment=${e.key}. Save to apply.`)
    })

  const moveFile = (b: (typeof binds)[number]) =>
    run(async () => {
      const n = name['b' + b.i] || secretName(`${stem}-${b.source.split('/').pop()}`)
      if ((await exists(n)) && !confirm(`Replace the value of the existing secret "${n}"?`)) return
      await api.setSecret({ name: n, fromFile: b.source, replace: true })
      const ls = [...lines]
      ls[b.i] = `Secret=${n},type=mount,target=${b.target}`
      onapply(ls.join('\n'), `Created secret "${n}" from ${b.source}. It is mounted read-only at ${b.target}; add uid=, gid= or mode= if the app needs them. Save to apply.`)
    })

  function addRef() {
    if (!existing) return
    const target = refTarget.trim()
    let line = `Secret=${existing},type=${refType}`
    if (target) line += `,target=${target}`
    onapply(insert([...lines], line).join('\n'), `Added a reference to "${existing}". Save to apply.`)
  }
</script>

<dialog class="wide" bind:this={dialog} onclose={onclose}>
  <h2>Secrets for <code>{path}</code></h2>
  <p class="muted">
    Values move into Podman's secret store and are never shown again. Secrets are created immediately; the file change
    lands in the editor for you to review and save.
  </p>

  <h3>Environment variables</h3>
  {#each envs as e (e.i)}
    <div class="row line">
      <code class="grow">{e.key}={mask(e.value)}</code>
      <input placeholder={secretName(`${stem}-${e.key}`)} bind:value={name['e' + e.i]} />
      <button disabled={busy} onclick={() => moveEnv(e)}>Move to secret</button>
    </div>
  {:else}
    <p class="muted small">No single-value <code>Environment=</code> lines.</p>
  {/each}

  <h3>Mounted files</h3>
  {#each binds as b (b.i)}
    <div class="row line">
      <code class="grow">{b.source} → {b.target}</code>
      <input placeholder={secretName(`${stem}-${b.source.split('/').pop()}`)} bind:value={name['b' + b.i]} />
      <button disabled={busy} onclick={() => moveFile(b)} title="Only for single files such as keys or certificates">Move to secret</button>
    </div>
  {:else}
    <p class="muted small">No bind mounts.</p>
  {/each}
  {#if binds.length}<p class="muted small">Only use this for single files such as keys; directories cannot become secrets.</p>{/if}

  <h3>Use an existing secret</h3>
  <div class="row wrap">
    <select bind:value={existing}>
      <option value="">Choose a secret</option>
      {#each catalog.secrets.filter((s) => s.exists) as s}<option value={s.name}>{s.name}</option>{/each}
    </select>
    <select bind:value={refType}>
      <option value="env">as environment variable</option>
      <option value="mount">as file</option>
    </select>
    <input class="grow" placeholder={refType === 'env' ? 'VARIABLE (default: secret name)' : '/run/secrets/<name>'} bind:value={refTarget} />
    <button disabled={!existing} onclick={addRef}>Insert</button>
  </div>

  {#if error}<div class="error-box">{error}</div>{/if}
  <div class="row foot">
    <span class="grow"></span>
    <button onclick={() => dialog.close()}>Close</button>
  </div>
</dialog>

<style>
  h3 {
    margin: 16px 0 8px;
    font-size: 14px;
  }
  .line {
    margin-bottom: 6px;
  }
  .line code {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .small {
    font-size: 12.5px;
  }
  .foot {
    margin-top: 16px;
  }
</style>
