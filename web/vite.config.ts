import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// `npm run dev` proxies to a quadstack backend on :8090
const backend = 'http://127.0.0.1:8090'

export default defineConfig({
  plugins: [svelte()],
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1500 },
  server: {
    proxy: {
      '/api': backend,
      '/auth': backend,
      '/ws': { target: backend, ws: true },
    },
  },
})
