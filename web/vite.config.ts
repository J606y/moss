import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: true,
    port: 5173,
    // 开发模式下 API 与 WebSocket 转发到本地 Go 服务端
    proxy: {
      '/api': { target: 'http://localhost:8787', ws: true },
      '/install.sh': { target: 'http://localhost:8787' },
      '/install.ps1': { target: 'http://localhost:8787' },
    },
  },
})
