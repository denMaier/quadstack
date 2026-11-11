<script lang="ts">
  import { onMount } from 'svelte'
  import { api, ApiError, type DeletedFile } from './api'

  // Deleted files keep their saved versions; this list brings one back or forgets it for good
  let { onrestored }: { onrestored: (path: string) => void } = $props()

  let files = $state<DeletedFile[]>([])
  let busy = $state('')
  let error = $state('')

  async function load() {
    try {
      files = await api.deletedFiles()
    } catch (e: any) {
      error = e.message
    }
  }
  onMount(load)

  async function restore(f: DeletedFile) {
    busy = f.path
    error = ''
    try {
      const [newest] = await api.history(f.path)
      const { content } = await api.version(f.path, newest.id)
      try {
        await api.restoreFile(f.path, content)
      } catch (e) {
        const output = e instanceof ApiError && e.status === 422 ? e.body?.validation?.output : undefined
        if (output === undefined || !confirm(`Podman's quadlet generator rejects the last version of ${f.path}:\n\n${output}\n\nRestore it anyway?`)) throw e
        await api.restoreFile(f.path, content, true)
      }
      await load()
      onrestored(f.path)
    } catch (e: any) {
      error = e.message
    } finally {
      busy = ''
    }
  }

  async function forget(f: DeletedFile) {
    if (!confirm(`Forget all ${f.versions} saved version(s) of ${f.path}? They cannot be restored afterwards.`)) return
    busy = f.path
    error = ''
    try {
      await api.forgetHistory(f.path)
      await load()
    } catch (e: any) {
      error = e.message
    } finally {
      busy = ''
    }
  }
</script>

{#if files.length || error}
  <h2>Deleted files</h2>
  {#if error}<div class="error-box">{error}</div>{/if}
  <div class="panel">
    <table>
      <thead><tr><th>File</th><th>Deleted</th><th>Versions</th><th></th></tr></thead>
      <tbody>
        {#each files as f (f.path)}
          <tr>
            <td><code>{f.path}</code></td>
            <td class="muted">{new Date(f.time).toLocaleString()}</td>
            <td class="muted">{f.versions}</td>
            <td class="actions">
              <button disabled={!!busy} onclick={() => restore(f)} title="Recreate the file from its last version">Restore</button>
              <button class="ghost danger" disabled={!!busy} onclick={() => forget(f)} title="Delete the saved versions, for example old credentials">Forget</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  h2 {
    margin: 20px 0 8px;
    font-size: 15px;
  }
  .actions {
    text-align: right;
    white-space: nowrap;
  }
</style>
