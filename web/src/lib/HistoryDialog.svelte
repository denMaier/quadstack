<script lang="ts">
  import { onMount } from 'svelte'
  import { MergeView } from '@codemirror/merge'
  import { EditorView } from '@codemirror/view'
  import { EditorState } from '@codemirror/state'
  import { StreamLanguage } from '@codemirror/language'
  import { properties } from '@codemirror/legacy-modes/mode/properties'
  import { oneDark } from '@codemirror/theme-one-dark'
  import { api, type Version } from './api'

  let {
    path,
    current,
    onclose,
    onrestore,
  }: { path: string; current: string; onclose: () => void; onrestore: (v: Version, text: string) => void } = $props()

  let dialog: HTMLDialogElement
  let diffHost = $state<HTMLDivElement>()
  let versions = $state<Version[]>([])
  let selected = $state<Version>()
  let text = $state('')
  let error = $state('')
  let merge: MergeView | undefined

  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches
  const ext = [StreamLanguage.define(properties), EditorState.readOnly.of(true), EditorView.editable.of(false), dark ? oneDark : []]

  onMount(() => {
    dialog.showModal()
    api
      .history(path)
      .then((vs) => {
        versions = vs
        if (vs.length) pick(vs[0])
      })
      .catch((e) => (error = e.message))
    return () => merge?.destroy()
  })

  async function pick(v: Version) {
    selected = v
    try {
      text = (await api.version(path, v.id)).content
    } catch (e: any) {
      error = e.message
      return
    }
    merge?.destroy()
    // Left: the saved version, right: what is in the editor now
    merge = new MergeView({
      a: { doc: text, extensions: ext },
      b: { doc: current, extensions: ext },
      parent: diffHost!,
      collapseUnchanged: { margin: 3, minSize: 6 },
    })
  }
</script>

<dialog class="wide" bind:this={dialog} onclose={onclose}>
  <h2>History of <code>{path}</code></h2>
  {#if error}<div class="error-box">{error}</div>{/if}
  {#if !versions.length && !error}
    <p class="muted">No earlier versions yet. Every save keeps the previous version here (the last 50).</p>
  {:else}
    <div class="layout">
      <ul>
        {#each versions as v (v.id)}
          <li>
            <button class:active={v === selected} onclick={() => pick(v)}>
              {new Date(v.time).toLocaleString()}
            </button>
          </li>
        {/each}
      </ul>
      <div class="diff">
        <div class="legend muted row"><span class="grow">Saved version</span><span class="grow">Current editor</span></div>
        <div bind:this={diffHost}></div>
      </div>
    </div>
  {/if}
  <div class="row foot">
    <span class="grow"></span>
    <button onclick={() => dialog.close()}>Close</button>
    <button class="primary" disabled={!selected} onclick={() => onrestore(selected!, text)}>Load this version</button>
  </div>
</dialog>

<style>
  .layout {
    display: grid;
    grid-template-columns: 190px 1fr;
    gap: 12px;
    min-height: 0;
  }
  ul {
    margin: 0;
    padding: 0;
    list-style: none;
    max-height: 60vh;
    overflow: auto;
  }
  li button {
    width: 100%;
    margin-bottom: 4px;
    font-size: 12.5px;
  }
  li button.active {
    border-color: var(--accent);
  }
  .diff {
    min-width: 0;
    max-height: 60vh;
    overflow: auto;
    border: 1px solid var(--border);
    border-radius: 6px;
    font-family: var(--mono);
  }
  .legend {
    padding: 4px 10px;
    font-size: 12px;
    border-bottom: 1px solid var(--border);
  }
  .foot {
    margin-top: 14px;
  }
  @media (max-width: 640px) {
    .layout {
      grid-template-columns: 1fr;
    }
    ul {
      max-height: 140px;
    }
  }
</style>
