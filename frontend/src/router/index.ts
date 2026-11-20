import { createRouter, createWebHistory, START_LOCATION } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import { readGalleryScroll } from '../utils/galleryScroll'

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

function restoreGalleryScroll(fullPath: string, position: { left: number; top: number }) {
  return new Promise<typeof position | false>(resolve => {
    const observer = new MutationObserver(check)
    const unregister = router.afterEach((target, _from, failure) => {
      if (!failure && target.fullPath !== fullPath) finish(false)
    })

    function finish(result: typeof position | false) {
      observer.disconnect()
      unregister()
      resolve(result)
    }

    function check() {
      if (router.currentRoute.value.fullPath !== fullPath) {
        finish(false)
      } else if (document.querySelector<HTMLElement>('[data-gallery-ready]')?.dataset.galleryReady === fullPath) {
        finish(position)
      }
    }

    observer.observe(document.body, { subtree: true, childList: true, attributeFilter: ['data-gallery-ready'] })
    check()
  })
}

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior(_to, from, savedPosition) {
    // 浏览器前进/后退（含从详情页返回）：等列表渲染完成后恢复到离开时的位置
    const isGallery = _to.path === '/gallery'
    const position = (isGallery && from === START_LOCATION ? readGalleryScroll(_to.fullPath) : undefined) ?? savedPosition
    if (position) {
      return isGallery && position.top > 0
        ? restoreGalleryScroll(_to.fullPath, position)
        : restoreScroll(position)
    }
    // 同一路由内的 query 变化（翻页/筛选）：不动滚动
    if (_to.path === from.path) return false
    return { top: 0 }
  },
  routes: [
    { path: '/', component: DashboardView },
    { path: '/submit', component: () => import('../views/DirectSubmitView.vue') },
    // 外部平台同步：单条录入别家生成的图 + 提示词
    { path: '/external', component: () => import('../views/ExternalImportView.vue') },
    { path: '/prompts', component: () => import('../views/PromptsView.vue') },
    { path: '/prompts/new', component: () => import('../views/PromptEditView.vue') },
    { path: '/prompts/:id', component: () => import('../views/PromptDetailView.vue') },
    { path: '/prompts/:id/edit', component: () => import('../views/PromptEditView.vue') },
    { path: '/json-files', component: () => import('../views/JsonFilesView.vue') },
    { path: '/workflows', component: () => import('../views/WorkflowsView.vue') },
    { path: '/tasks', component: () => import('../views/TaskListView.vue') },
    { path: '/tasks/:id', component: () => import('../views/TaskDetailView.vue') },
    { path: '/gallery', component: () => import('../views/GalleryView.vue') },
    // 条件式批量清理 + 回收站。破坏性操作，但走「预览 → 确认」两步且有回收站兜底。
    // 懒加载：这个页面是全项目唯一用 Element Plus 的，Element 的 chunk 约 400 kB，
    // 静态 import 会让**所有页面**的首屏都背上它。改成动态 import 后，
    // 只有真正进这个页面才会下载，其它页面完全不受影响。
    { path: '/maintenance', component: () => import('../views/MaintenanceView.vue') },
    // 脚本运行台：只在后端 ENABLE_SCRIPTS 开启时有内容，关闭时页面给开启说明、
    // 侧边栏也不出现。同样懒加载（这一页载入频率低，没必要进首屏包）。
    { path: '/scripts', component: () => import('../views/ScriptsView.vue') },
    { path: '/settings', component: () => import('../views/SettingsView.vue') },
    // 只在后端 DEBUG 开启时才有内容；没开时页面会给一份开启说明，
    // 侧边栏入口也不会出现
    { path: '/debug', component: () => import('../views/DebugView.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

export default router
