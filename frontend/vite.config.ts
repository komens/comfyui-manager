import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import AutoImport from 'unplugin-auto-import/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// 后端端口跟随 SERVER_PORT 环境变量（默认 8080），与 backend/docker 保持一致
const backendPort = process.env.SERVER_PORT || '8080'

export default defineConfig({
  plugins: [
    vue(),
    // Element Plus 按需导入：只打进模板里真正用到的组件与其样式。
    // 不配这个的话 `import 'element-plus/theme-chalk/index.css'` 会把整份样式
    // （1667 个模块）全打进来 —— 实测 CSS 从 47.7 kB 涨到 411 kB。
    AutoImport({
      resolvers: [ElementPlusResolver()],
    }),
    Components({
      resolvers: [ElementPlusResolver()],
      dts: false,
    }),
  ],
  build: {
    // 不要在这里手动 split element-plus chunk。
    // 实测：手动提出来之后 Vite 会因为它注册了全局 CSS 变量而判定「有副作用」，
    // 于是在 index.html 里加 modulepreload —— 首屏照样把它全量下载，
    // 拆分等于白做（首页仍要预加载 333 kB gzip 的 element-plus JS）。
    //
    // 靠路由懒加载自然切分即可：Element Plus 只被 MaintenanceView 引用，
    // 而该路由是动态 import，所以不进这个页面就不会下载它。
    chunkSizeWarningLimit: 900,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': `http://localhost:${backendPort}`,
    },
  },
})
