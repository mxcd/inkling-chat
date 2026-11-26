import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The build goes where the Go binary embeds it (internal/web). In development this server
// proxies the API to the Go server; the rule is anchored, so a frontend route that merely
// starts with "api" stays here (DEP-001).
export default defineConfig({
  plugins: [vue()],
  build: { outDir: '../internal/web/html/dist', emptyOutDir: true },
  server: { proxy: { '^/api/': 'http://127.0.0.1:8080' } },
})
