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
  build: {
    rollupOptions: {
      output: {
        // 把稳定的框架依赖单独成块：业务代码改动后用户无需重下 react 运行时。
        // recharts 仅 ServerDetail 引用，已随其懒加载自然落到按需异步 chunk，无需在此列出。
        manualChunks: {
          'react-vendor': ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
})
