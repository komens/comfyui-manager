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

export type Workflow = {
  id: number
  name: string
  description: string
  workflow_path?: string
  workflow_json?: any
  mapping?: any
  negative_prompt?: string
  params_schema?: any
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
