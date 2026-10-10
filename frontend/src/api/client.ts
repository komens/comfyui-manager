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

/** 数据整理：各表当前规模 + 磁盘占用 + 孤儿文件。 */
export type CleanupStats = {
  counts: Record<string, number>
  /** images.created_at 的最早/最新值（Go 的 time 字符串，不能直接喂给 Date） */
  images_created_range: [string, string]
  favorite_images: number
  favorite_prompts: number
  files_on_disk: number
  image_bytes: number
  /** 磁盘上有、但库里没有任何记录引用的图片文件名 */
  orphan_files: string[]
  db_bytes: number
  db_path: string
}

/** 数据整理预览的结果。blocked 非空表示命中数低于安全阀，已阻止执行。 */
export type CleanupPreview = {
  target: 'images' | 'tasks' | 'prompts'
  stats: {
    images: number
    tasks: number
    items: number
    prompts: number
    bytes: number
    /** 因收藏被保护而没删的数量 */
    protected: number
    protected_running: number
    files_found: number
  }
  /** 本次操作的主体数量（图片数/任务数/提示词数，取决于 target） */
  primary_count: number
  samples: { id: number; title: string; filename: string; group: string; created_at: string }[]
  only_unfavorite: boolean
  warning: string
  blocked?: string
}

/** 回收站里的一个清理批次。 */
export type TrashBatch = {
  id: number
  /** 人类可读的条件说明，如 "images时间 2026-09-01~2026-09-30" */
  reason: string
  deleted_images: number
  bytes: number
  created_at: string
}
