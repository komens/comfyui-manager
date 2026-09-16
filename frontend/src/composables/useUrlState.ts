import { reactive, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

type UrlPrimitive = string | number | boolean

export interface UseUrlStateOptions {
  /**
   * 外部导航改变了当前页的 query（例如点击侧边栏菜单重新进入同一路由、浏览器前进/后退到不同 query）时调用。
   * 此时 state 已被重置为 URL 中的值，视图通常需要清空选中项并重新加载数据。
   */
  onExternalSync?: () => void
}

/**
 * 将列表页状态（页码、每页条数、筛选条件）与 URL query 双向同步：
 * - setup 时从当前 URL 读取初始化值，因此从详情页返回、刷新、分享链接都能停留在原分页/筛选；
 * - state 变化时用 router.replace 写回 URL（等于默认值的键会省略，保持 URL 干净；不新增历史记录）；
 * - 非默认值/非法值的 query 参数做类型转换与范围校验（page>=1，page_size 1..200，与后端 parsePagination 一致）。
 */
export function useUrlState<T extends Record<string, UrlPrimitive>>(
  defaults: T,
  options: UseUrlStateOptions = {},
): T {
  const route = useRoute()
  const router = useRouter()
  const ownPath = route.path

  function parseNumber(key: string, raw: string): number | null {
    const n = Number(raw)
    if (!Number.isInteger(n)) return null
    if (key === 'page' && n < 1) return null
    if (key === 'page_size' && (n < 1 || n > 200)) return null
    return n
  }

  /** 把任意 query 记录解析成完整的 state 值（缺失/非法的键回退默认值） */
  function parse(query: Record<string, unknown>): T {
    const out: Record<string, UrlPrimitive> = { ...defaults }
    for (const key of Object.keys(defaults)) {
      const raw = query[key]
      if (typeof raw !== 'string' || raw === '') continue
      const def = defaults[key as keyof T]
      if (typeof def === 'number') {
        const n = parseNumber(key, raw)
        if (n !== null) out[key] = n
      } else if (typeof def === 'boolean') {
        out[key] = raw === '1' || raw === 'true'
      } else {
        out[key] = raw
      }
    }
    return out as T
  }

  /** 序列化当前 state 为 query（省略等于默认值/空值/false 的键） */
  function serialize(): Record<string, string> {
    const q: Record<string, string> = {}
    for (const key of Object.keys(defaults)) {
      const value = state[key as keyof T]
      if (value === defaults[key as keyof T] || value === '' || value === false) continue
      q[key] = String(value)
    }
    return q
  }

  /** 保留本视图不托管的其它 query 参数，避免 replace 时被误删 */
  function unmanagedQuery(): Record<string, string> {
    const rest: Record<string, string> = {}
    for (const key of Object.keys(route.query)) {
      if (key in defaults) continue
      const raw = route.query[key]
      if (typeof raw === 'string' && raw !== '') rest[key] = raw
    }
    return rest
  }

  /** 把 query 记录规范化为可比较的字符串键（只保留非空字符串值，按 key 排序） */
  function queryKey(q: Record<string, string>): string {
    return JSON.stringify(Object.keys(q).sort().map(k => [k, q[k]]))
  }

  /** 当前路由 query 的规范化字符串视图（数组/空值视为不存在） */
  function routeQueryRecord(): Record<string, string> {
    const current: Record<string, string> = {}
    for (const key of Object.keys(route.query)) {
      const raw = route.query[key]
      if (typeof raw === 'string' && raw !== '') current[key] = raw
    }
    return current
  }

  const state = reactive(parse(route.query)) as T

  // 我们自己 replace 产生的导航会排队生效；连续快速翻页时，中间态的 query
  // 不应被当成外部导航反向覆盖 state（否则会出现旧页码闪回），用队列逐一消费。
  const ownNavigations: string[] = []

  // state 变化 → 写回 URL（replace 不产生新历史记录，浏览器返回仍然回到详情页那条记录）
  watch(state, () => {
    const query = { ...unmanagedQuery(), ...serialize() }
    const key = queryKey(query)
    if (key === queryKey(routeQueryRecord())) return
    ownNavigations.push(key)
    router.replace({ query })
  })

  // URL 变化 → 同步回 state。只在同一页面路径内处理：
  // 自己 replace 产生的变化由 ownNavigations 消费，剩下的都是外部导航（侧边栏、前进/后退、手改地址栏）。
  watch(() => route.query, () => {
    if (route.path !== ownPath) return
    const idx = ownNavigations.indexOf(queryKey(routeQueryRecord()))
    if (idx !== -1) {
      ownNavigations.splice(idx, 1)
      return
    }
    Object.assign(state, parse(route.query))
    options.onExternalSync?.()
  })

  return state
}
