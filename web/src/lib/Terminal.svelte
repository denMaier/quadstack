<script lang="ts">
  import { onMount } from 'svelte'
  import { Terminal } from '@xterm/xterm'
  import { FitAddon } from '@xterm/addon-fit'

  // Connects xterm.js to one of the /ws endpoints; a new url opens a new session
  let {
    url,
    interactive = false,
    onexit,
  }: { url: string; interactive?: boolean; onexit?: (code: number) => void } = $props()

  let host: HTMLDivElement
  let term: Terminal
  let fit: FitAddon
  let ws: WebSocket | undefined

  const dark = window.matchMedia('(prefers-color-scheme: dark)')
  const themes = {
    dark: { background: '#14161a', foreground: '#d7dae0', cursor: '#d7dae0', selectionBackground: '#3a4252' },
    light: { background: '#fbfbfc', foreground: '#1f2329', cursor: '#1f2329', selectionBackground: '#cfd8ea' },
  }

  function connect(u: string) {
    ws?.close()
    term.reset()
    const sock = new WebSocket(u)
    sock.binaryType = 'arraybuffer'
    sock.onopen = () => sock.send(JSON.stringify({ cols: term.cols, rows: term.rows }))
    sock.onmessage = (e) => {
      if (typeof e.data === 'string') {
        const msg = JSON.parse(e.data)
        if (msg.error) term.writeln(`\r\n\x1b[31m${msg.error}\x1b[0m`)
        if (msg.exit !== undefined && msg.exit !== 0) term.writeln(`\r\n\x1b[2m[exited with ${msg.exit}]\x1b[0m`)
        onexit?.(msg.exit)
        return
      }
      term.write(new Uint8Array(e.data))
    }
    sock.onclose = (e) => {
      if (e.code !== 1000 && ws === sock) term.writeln('\r\n\x1b[2m[connection closed]\x1b[0m')
    }
    ws = sock
  }

  onMount(() => {
    term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize: 12.5,
      convertEol: true,
      scrollback: 5000,
      cursorBlink: interactive,
      disableStdin: !interactive,
      theme: dark.matches ? themes.dark : themes.light,
    })
    fit = new FitAddon()
    term.loadAddon(fit)
    term.open(host)
    fit.fit()
    term.onData((d) => ws?.readyState === WebSocket.OPEN && ws.send(new TextEncoder().encode(d)))
    term.onResize(({ cols, rows }) => ws?.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ cols, rows })))
    const ro = new ResizeObserver(() => fit.fit())
    ro.observe(host)
    const onTheme = () => (term.options.theme = dark.matches ? themes.dark : themes.light)
    dark.addEventListener('change', onTheme)
    if (interactive) term.focus()
    return () => {
      ro.disconnect()
      dark.removeEventListener('change', onTheme)
      ws?.close()
      term.dispose()
    }
  })

  $effect(() => {
    if (term && url) connect(url)
  })
</script>

<div class="term" bind:this={host}></div>

<style>
  .term {
    height: 100%;
    min-height: 0;
    padding: 6px 8px;
    background: var(--term-bg);
    border-radius: var(--radius);
    border: 1px solid var(--border);
    overflow: hidden;
  }
</style>
