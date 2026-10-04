const BASE = ''

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const url = path.startsWith('http') ? path : BASE + path
  const response = await fetch(url, init)
  if (!response.ok) {
    let message = response.statusText
    try {
      const body = await response.json()
      if (body?.error) message = body.error
    } catch {
      // 非 JSON 响应，使用 statusText
    }
    throw new Error(message)
  }
  try {
    return await response.json() as T
  } catch {
    throw new Error('服务器返回了无效的响应格式')
  }
}

/** 一处提示词落点：某个节点的某个字段。field 可能为空（老格式离线解析不出来）。 */
export type PromptTarget = {
  node_id: string
  field?: string
}

/** 编辑器下拉框里的一个候选提示词节点。 */
export type PromptCandidate = {
  node_id: string
  class_type: string
  field?: string
  preview: string
  /** 自动识别给出的角色提示 */
  role?: 'positive' | 'negative' | ''
}

/**
 * 正负向提示词该写到哪。
 * negative_shared 为真表示负向与正向落在同一处 —— 工作流把负向从正向派生出来，
 * 没有独立的负向文本框，负向提示词不会生效。
 */
export type PromptTargets = {
  positive: PromptTarget | null
  negative: PromptTarget | null
  negative_shared: boolean
}

export type PromptTargetsResult = {
  detected: PromptTargets
  stored: { positive_prompt?: PromptTarget; negative_prompt?: PromptTarget }
  candidates: PromptCandidate[]
  /** 负向写不进去的原因，空串表示没有这个问题 */
  negative_unavailable: string
}

export type Workflow = {
  id: number
  name: string
  description: string
  workflow_path?: string
  workflow_json?: any
  /** 只含 positive_prompt / negative_prompt 两个落点 */
  mapping?: any
  negative_prompt?: string
  enabled: boolean
  is_default?: boolean
  /** 历史任务数（含已完成），删除前用于提示影响范围 */
  task_count?: number
  /** 排队中/生成中的任务数，>0 时后端禁止删除 */
  in_flight?: number
  created_at?: string
  updated_at?: string
}

export type Task = {
  id: number
  source_type: string
  workflow_id: number
  workflow_name?: string
  comfyui_url?: string
  parameters?: any
  status: string
  total_count: number
  success_count: number
  failed_count: number
  created_at: string
  started_at?: string
  completed_at?: string
  /** 列表接口附加：代表结果图 / 关联提示词 */
  image_id?: number
  prompt_id?: number
  prompt_title?: string
  prompt_count?: number
}

/** 一条工作流校验发现：节点 + 字段 + 原因，可直接渲染成给人看的清单 */
export type ValidationIssue = {
  node_id: string
  class_type: string
  field?: string
  /**
   * errors 里：node_type_missing / value_not_in_list / required_missing / unknown_field /
   * workflow_format；warnings 里还可能是 widget_values_mismatch / auto_repairable
   * （auto_repairable = 导出把控件值串位了，提交时会被自动挪回原位）
   */
  kind: string
  message: string
  value?: string
  candidates?: string[]
}

/**
 * 提交前校验的结果。
 * unreachable=true 表示「连不上 ComfyUI」，与「工作流本身有问题」是两回事，
 * 前端必须分开呈现，否则服务没起时会显示成一堆「节点不存在」。
 */
export type ValidationResult = {
  ok: boolean
  unreachable?: boolean
  message?: string
  workflow_id: number
  workflow_name: string
  comfyui_url: string
  checked_at: string
  errors: ValidationIssue[]
  warnings: ValidationIssue[]
  /** 提交时会被自动跳过的节点（前端专用节点、旁路/静音节点）。不是错误，但要看得见。 */
  skipped_nodes?: { node_id: string; class_type: string; reason: string }[]
}

/**
 * 一个调试产物文件。
 * DEBUG 关闭时 /api/debug/status 返回 404（路由根本没注册），
 * 调用方据 catch 判定「未开启」，不要当成错误弹红字。
 */
export type DebugFile = {
  name: string
  bytes: number
  exists: boolean
  modified?: string
  /** 已达滚动上限，再写就会覆盖成 .1 */
  truncated?: boolean
}

export type DebugStatus = {
  on: boolean
  /** 落盘目录的绝对路径（云端部署时即宿主机可读的那个挂载目录） */
  dir: string
  files: DebugFile[]
}
