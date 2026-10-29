<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Stack } from './api'

  let { stacks, onclose, ondone }: { stacks: Stack[]; onclose: () => void; ondone: (stackID: string) => void } = $props()

  let dialog: HTMLDialogElement
  let input = $state('')
  let files = $state<{ name: string; content: string }[]>([])
  let target = $state('')
  let error = $state('')
  let busy = $state(false)

  const folders = $derived(stacks.filter((s) => s.id.startsWith('d/')))

  onMount(() => dialog.showModal())

  // podlet prints each file as "# name.ext" followed by its content, files separated by "---"
  function parse(out: string) {
    return out
      .split(/^---\s*$/m)
      .map((part) => part.trim())
      .filter(Boolean)
      .map((part, i) => {
        const m = part.match(/^#\s*(\S+\.\w+)\s*\n/)
        return m ? { name: m[1], content: part.slice(m[0].length) + '\n' } : { name: `imported-${i + 1}.container`, content: part + '\n' }
      })
  }

  async function convert() {
    busy = true
    error = ''
    try {
      files = parse((await api.convert(input)).output)
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  async function create() {
    const dir = target.trim()
    if (!dir) {
      error = 'Choose or name a stack folder.'
      return
    }
    busy = true
    error = ''
    try {
      if (!folders.some((s) => s.name === dir)) await api.createStack(dir)
      for (const f of files) await api.createFile(dir + '/' + f.name, f.content)
      ondone('d/' + dir)
    } catch (e: any) {
      error = e.body?.validation?.output || e.message
    } finally {
      busy = false
    }
  }
</script>

<dialog class="wide" bind:this={dialog} onclose={onclose}>
  <h2>Import from docker run or compose</h2>
  {#if !files.length}
    <label class="field">
      Paste a <code>docker run …</code> / <code>podman run …</code> command or a compose file
      <textarea rows="14" bind:value={input} placeholder="docker run -d --name web -p 8080:80 nginx"></textarea>
    </label>
  {:else}
    {#each files as f, i}
      <label class="field">
        <input bind:value={files[i].name} class="mono" />
        <textarea rows={Math.min(16, f.content.split('\n').length + 1)} bind:value={files[i].content}></textarea>
      </label>
    {/each}
    <label class="field">
      Stack folder
      <input list="stack-folders" bind:value={target} placeholder="new or existing folder" />
      <datalist id="stack-folders">
        {#each folders as s}<option value={s.name}></option>{/each}
      </datalist>
    </label>
  {/if}
  {#if error}<div class="error-box">{error}</div>{/if}
  <div class="row foot">
    {#if files.length}<button class="ghost" onclick={() => (files = [])}>‹ Back</button>{/if}
    <span class="grow"></span>
    <button onclick={() => dialog.close()}>Cancel</button>
    {#if files.length}
      <button class="primary" disabled={busy} onclick={create}>Create files</button>
    {:else}
      <button class="primary" disabled={busy || !input.trim()} onclick={convert}>Convert</button>
    {/if}
  </div>
</dialog>

<style>
  textarea {
    width: 100%;
    resize: vertical;
  }
  .foot {
    margin-top: 14px;
  }
</style>
