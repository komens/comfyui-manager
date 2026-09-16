import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 后端端口跟随 SERVER_PORT 环境变量（默认 8080），与 backend/docker 保持一致
const backendPort = process.env.SERVER_PORT || '8080'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': `http://localhost:${backendPort}`,
    },
  },
})
