<script lang="ts">
  import { onMount } from 'svelte'
  import { basicSetup } from 'codemirror'
  import { EditorView, keymap } from '@codemirror/view'
  import { Compartment, EditorState } from '@codemirror/state'
  import { StreamLanguage } from '@codemirror/language'
  import { properties } from '@codemirror/legacy-modes/mode/properties'
  import { autocompletion, type CompletionContext } from '@codemirror/autocomplete'
  import { forceLinting, linter, type Diagnostic } from '@codemirror/lint'
  import { oneDark } from '@codemirror/theme-one-dark'
  import { sections } from './quadlet'

  let {
    value = $bindable(''),
    problems = '',
    onsave,
  }: { value?: string; problems?: string; onsave?: () => void } = $props()

  let host: HTMLDivElement
  let view: EditorView
  const theme = new Compartment()
  const dark = window.matchMedia('(prefers-color-scheme: dark)')

  // Completes [Section] headers and the keys of the section the cursor is in
  function complete(ctx: CompletionContext) {
    const line = ctx.state.doc.lineAt(ctx.pos)
    const before = line.text.slice(0, ctx.pos - line.from)
    if (/^\s*\[\w*$/.test(before)) {
      const from = line.from + before.indexOf('[') + 1
      return { from, options: Object.keys(sections).map((s) => ({ label: s, apply: s + ']', type: 'namespace' })) }
    }
    const word = ctx.matchBefore(/^\s*\w*$/)
    if (!word || (word.from === word.to && !ctx.explicit)) return null
    let section = ''
    for (let n = line.number - 1; n >= 1; n--) {
      const m = ctx.state.doc.line(n).text.match(/^\s*\[(\w+)\]/)
      if (m) {
        section = m[1]
        break
      }
    }
    const keys = sections[section]
    if (!keys) return null
    return {
      from: word.from + (word.text.length - word.text.trimStart().length),
      options: keys.map((k) => ({ label: k, apply: k + '=', type: 'property' })),
    }
  }

  // quadlet's messages rarely carry line numbers; point at the key they name when they do name one
  function diagnostics(view: EditorView): Diagnostic[] {
    if (!problems) return []
    const out: Diagnostic[] = []
    for (const msg of problems.split('\n').filter(Boolean)) {
      const key = msg.match(/key '?"?([A-Za-z0-9]+)/)?.[1]
      let from = 0
      let to = Math.min(view.state.doc.line(1).to, view.state.doc.length)
      if (key) {
        for (let n = 1; n <= view.state.doc.lines; n++) {
          const l = view.state.doc.line(n)
          if (l.text.trimStart().startsWith(key + '=')) {
            from = l.from
            to = l.to
            break
          }
        }
      }
      out.push({ from, to, severity: 'error', message: msg })
    }
    return out
  }

  onMount(() => {
    view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          StreamLanguage.define(properties),
          autocompletion({ override: [complete] }),
          linter(diagnostics, { delay: 0 }),
          theme.of(dark.matches ? oneDark : []),
          keymap.of([{ key: 'Mod-s', preventDefault: true, run: () => (onsave?.(), true) }]),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) value = u.state.doc.toString()
          }),
        ],
      }),
    })
    const onTheme = () => view.dispatch({ effects: theme.reconfigure(dark.matches ? oneDark : []) })
    dark.addEventListener('change', onTheme)
    return () => {
      dark.removeEventListener('change', onTheme)
      view.destroy()
    }
  })

  // Outside changes (loading a file, restoring a version) replace the document
  $effect(() => {
    if (view && value !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } })
    }
  })

  // Re-run the linter when the validation result changes
  $effect(() => {
    problems
    if (view) forceLinting(view)
  })
</script>

<div class="editor" bind:this={host}></div>

<style>
  .editor {
    height: 100%;
    min-height: 0;
    overflow: hidden;
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .editor :global(.cm-editor) {
    height: 100%;
    font-size: 13px;
  }
  .editor :global(.cm-scroller) {
    font-family: var(--mono);
  }
</style>
