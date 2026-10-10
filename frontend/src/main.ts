import { createApp } from 'vue'
import App from './App.vue'
import router from './router'
import './style.css'
// ⚠️ 顺序要求：Element Plus 的组件样式 → 本项目主题覆盖 → 全局变量。
// 主题覆盖必须在 style.css 之后、且不能被 Element 的样式盖住，
// 否则主色会退回 Element 默认的 #409EFF。
import './element-theme.css'

/**
 * Element Plus 走 unplugin-vue-components 按需自动注册（见 vite.config.ts），
 * 所以这里**不要** app.use(ElementPlus)、也不要手动 component() 一堆，
 * 更不要 import 'element-plus/theme-chalk/index.css'——那会把整份样式全打包进去。
 *
 * 只需要在 CSS 里做主题对齐：见 element-theme.css。
 */

createApp(App).use(router).mount('#app')

