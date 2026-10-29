<script lang="ts">
  import { onMount } from 'svelte'
  import { api } from './api'
  import { catalog, loadSecrets } from './catalog.svelte'

  let error = $state('')
  let busy = $state(false)
  // The create/replace form; name is fixed when replacing
  let form = $state<{ name: string; value: string; replace: boolean }>()
  let reveal = $state(false)
  let dialog = $state<HTMLDialogElement>()

  async function load() {
    error = ''
    try {
      catalog.secrets = await api.secrets()
    } catch (e: any) {
      error = e.message
    }
  }

  function open(name = '', replace = false) {
    form = { name, value: '', replace }
    reveal = false
    queueMicrotask(() => dialog?.showModal())
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    busy = true
    error = ''
    try {
      await api.setSecret({ name: form!.name.trim(), value: form!.value, replace: form!.replace })
      dialog?.close()
      await load()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = false
    }
  }

  async function remove(name: string, used: number) {
    const msg = used
      ? `"${name}" is still referenced by ${used} file(s); those units will fail to start. Delete anyway?`
      : `Delete the secret "${name}"? Its value cannot be recovered.`
    if (!confirm(msg)) return
    try {
      await api.removeSecret(name, used > 0)
      await load()
    } catch (e: any) {
      error = e.message
    }
  }

  const fmt = (t?: string) => (t ? new Date(t).toLocaleString() : '')

  onMount(loadSecrets)
</script>

<section>
  <div class="row wrap head">
    <h1 class="grow">Secrets</h1>
    <button class="primary" onclick={() => open()}>+ New secret</button>
  </div>
  <p class="muted">
    Podman secrets for this account. Containers use them with <code>Secret=name,type=env,target=VAR</code> or as a
    read-only file with <code>Secret=name,type=mount,target=/path</code>. Values can be replaced but never read back.
    Running containers keep the old value until they restart.
  </p>
  {#if error && !form}<div class="error-box">{error}</div>{/if}

  <div class="panel scroll">
    <table>
      <thead><tr><th>Name</th><th>Used by</th><th>Updated</th><th></th></tr></thead>
      <tbody>
        {#each catalog.secrets as s (s.name)}
          <tr>
            <td>
              <code>{s.name}</code>
              {#if !s.exists}<div class="missing">Missing: referenced but not in Podman</div>{/if}
            </td>
            <td>
              {#each s.usedBy as u}
                <div class="small">
                  <a href={'#/stack/' + encodeURIComponent(u.stack)}>{u.path}</a>
                  <span class="muted">as {u.type === 'env' ? '$' + u.target : u.target}</span>
                </div>
              {:else}
                <span class="muted small">unused</span>
              {/each}
            </td>
            <td class="small muted">{fmt(s.updated || s.created)}</td>
            <td class="actions">
              {#if s.exists}
                <button onclick={() => open(s.name, true)}>Replace value</button>
                <button class="ghost danger" title="Delete" onclick={() => remove(s.name, s.usedBy.length)}>🗑</button>
              {:else}
                <button class="primary" onclick={() => open(s.name)}>Create</button>
              {/if}
            </td>
          </tr>
        {:else}
          <tr><td colspan="4" class="muted">No secrets yet. Create one here, or use <b>Secrets</b> in a container's editor to move an <code>Environment=</code> value or a mounted key file into one.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>
</section>

{#if form}
  <dialog bind:this={dialog} onclose={() => ((form = undefined), (error = ''))}>
    <h2>{form.replace ? `Replace "${form.name}"` : 'New secret'}</h2>
    <form onsubmit={submit} autocomplete="off">
      {#if !form.replace}
        <label class="field">
          Name
          <input bind:value={form.name} required pattern="[A-Za-z0-9][A-Za-z0-9_.\-]*" placeholder="e.g. sonarr-api-key" />
        </label>
      {/if}
      <label class="field">
        <span class="row">Value <span class="grow"></span><button type="button" class="ghost" onclick={() => (reveal = !reveal)}>{reveal ? 'Hide' : 'Show'}</button></span>
        {#if reveal}
          <textarea rows="6" bind:value={form.value} required spellcheck="false"></textarea>
        {:else}
          <input type="password" bind:value={form.value} required autocomplete="new-password" />
        {/if}
      </label>
      {#if error}<div class="error-box">{error}</div>{/if}
      <div class="row foot">
        <span class="grow"></span>
        <button type="button" onclick={() => dialog?.close()}>Cancel</button>
        <button class="primary" type="submit" disabled={busy}>{form.replace ? 'Replace' : 'Create'}</button>
      </div>
    </form>
  </dialog>
{/if}

<style>
  h1 {
    margin: 0;
    font-size: 20px;
  }
  .head {
    margin-bottom: 8px;
  }
  .scroll {
    overflow-x: auto;
  }
  .small {
    font-size: 12.5px;
  }
  .missing {
    color: var(--bad);
    font-size: 12px;
  }
  .actions {
    text-align: right;
    white-space: nowrap;
  }
  textarea {
    width: 100%;
  }
  .foot {
    margin-top: 14px;
  }
</style>
