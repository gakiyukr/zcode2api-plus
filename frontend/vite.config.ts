import { fileURLToPath } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    // 開發模式把 API 與 meta 代理到本機 FastAPI
    proxy: {
      '/admin/api': 'http://127.0.0.1:3000',
      '/guest/api': 'http://127.0.0.1:3000',
      '/meta': 'http://127.0.0.1:3000',
      '/v1': 'http://127.0.0.1:3000',
      '/async': 'http://127.0.0.1:3000',
    },
  },
})
