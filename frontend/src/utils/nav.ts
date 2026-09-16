import type { Router } from 'vue-router'

/**
 * 优先走浏览器历史返回：如果上一条历史记录正好是目标列表页（可能带 ?page=3&... 等分页/筛选 query），
 * back() 能原样恢复列表的 URL 与滚动位置；没有可用历史（直链/新标签打开）或上一页不是该列表时，
 * 回退为跳转列表第一页。
 */
export function backToListOr(router: Router, listPath: string) {
  const back = (window.history.state as { back?: string | null } | null)?.back
  if (back && back.split('?')[0] === listPath) {
    router.back()
    return
  }
  router.push(listPath)
}
