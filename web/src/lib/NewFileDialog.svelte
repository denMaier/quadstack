<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Stack } from './api'
  import { kindsForNew, template } from './quadlet'

  let { stack, onclose, ondone }: { stack: Stack; onclose: () => void; ondone: (path: string) => void } = $props()

  let dialog: HTMLDialogElement
  let kind = $state('container')
  let name = $state('')
  let error = $state('')

  const pod = $derived(stack.files.find((f) => f.kind === 'pod'))
  const podRef = $derived(pod ? pod.path.split('/').pop() : undefined)
  let joinPod = $state(true)
  const path = $derived(name ? (stack.dir ? stack.dir + '/' : '') + name + '.' + kind : '')

  onMount(() => {
    // Top-level stacks are named after their pod or unit, a good default for the first file
    if (!stack.files.length) name = stack.name
    dialog.showModal()
  })

  async function create(e: SubmitEvent) {
    e.preventDefault()
    error = ''
    if (!/^[A-Za-z0-9][A-Za-z0-9_.@-]*$/.test(name)) {
      error = 'Use letters, digits, dot, dash, underscore or @.'
      return
    }
    try {
      await api.createFile(path, template(kind, name, kind === 'container' && joinPod ? podRef : undefined))
      ondone(path)
    } catch (e: any) {
      error = e.body?.validation?.output || e.message
    }
  }
</script>

<dialog bind:this={dialog} onclose={onclose}>
  <h2>New file in {stack.name}</h2>
  <form onsubmit={create}>
    <label class="field">
      Type
      <select bind:value={kind}>
        {#each kindsForNew as k}<option value={k}>.{k}</option>{/each}
      </select>
    </label>
    <label class="field">
      Name
      <input bind:value={name} placeholder="e.g. jellyfin" required />
    </label>
    {#if kind === 'container' && podRef}
      <label class="row check"><input type="checkbox" bind:checked={joinPod} /> Join the pod <code>{podRef}</code></label>
    {/if}
    {#if path}<p class="muted">Creates <code>{path}</code> from a template and reloads systemd.</p>{/if}
    {#if error}<div class="error-box">{error}</div>{/if}
    <div class="row foot">
      <span class="grow"></span>
      <button type="button" onclick={() => dialog.close()}>Cancel</button>
      <button class="primary" type="submit">Create</button>
    </div>
  </form>
</dialog>

<style>
  .check {
    margin-bottom: 12px;
  }
  .foot {
    margin-top: 14px;
  }
</style>
