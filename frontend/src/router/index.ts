import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import DirectSubmitView from '../views/DirectSubmitView.vue'
import JsonFilesView from '../views/JsonFilesView.vue'
import WorkflowsView from '../views/WorkflowsView.vue'
import SettingsView from '../views/SettingsView.vue'
import TaskDetailView from '../views/TaskDetailView.vue'
import GalleryView from '../views/GalleryView.vue'
import PromptsView from '../views/PromptsView.vue'
import PromptDetailView from '../views/PromptDetailView.vue'
import PromptEditView from '../views/PromptEditView.vue'
import TaskListView from '../views/TaskListView.vue'
import DebugView from '../views/DebugView.vue'

// SPA 异步渲染下浏览器自动恢复滚动不可靠，关闭后交给下面的 scrollBehavior 延迟恢复
if ('scrollRestoration' in window.history) {
  window.history.scrollRestoration = 'manual'
}

/** 返回列表页时数据是异步加载的，等内容高度足够容纳目标滚动位置后再真正滚动（约 1.2s 后放弃等待） */
function restoreScroll(position: { left: number; top: number }) {
  return new Promise((resolve) => {
    let tries = 0
    const check = () => {
      tries += 1
      const scrollable = document.documentElement.scrollHeight - window.innerHeight
      if (scrollable >= position.top || tries > 30) resolve(position)
      else setTimeout(check, 40)
    }
    check()
  })
}

export default createRouter({
  history: createWebHistory(),
  scrollBehavior(_to, from, savedPosition) {
    // 浏览器前进/后退（含从详情页返回）：等列表渲染完成后恢复到离开时的位置
    if (savedPosition) return restoreScroll(savedPosition)
    // 同一路由内的 query 变化（翻页/筛选）：不动滚动
    if (_to.path === from.path) return false
    return { top: 0 }
  },
  routes: [
    { path: '/', component: DashboardView },
    { path: '/submit', component: DirectSubmitView },
    { path: '/prompts', component: PromptsView },
    { path: '/prompts/new', component: PromptEditView },
    { path: '/prompts/:id', component: PromptDetailView },
    { path: '/prompts/:id/edit', component: PromptEditView },
    { path: '/json-files', component: JsonFilesView },
    { path: '/workflows', component: WorkflowsView },
    { path: '/tasks', component: TaskListView },
    { path: '/tasks/:id', component: TaskDetailView },
    { path: '/gallery', component: GalleryView },
    // 条件式批量清理 + 回收站。破坏性操作，但走「预览 → 确认」两步且有回收站兜底。
    // 懒加载：这个页面是全项目唯一用 Element Plus 的，Element 的 chunk 约 400 kB，
    // 静态 import 会让**所有页面**的首屏都背上它。改成动态 import 后，
    // 只有真正进这个页面才会下载，其它页面完全不受影响。
    { path: '/maintenance', component: () => import('../views/MaintenanceView.vue') },
    { path: '/settings', component: SettingsView },
    // 只在后端 DEBUG 开启时才有内容；没开时页面会给一份开启说明，
    // 侧边栏入口也不会出现
    { path: '/debug', component: DebugView },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})
