export async function api<T>(path: string, options?: RequestInit): Promise<T> {
  const response = await fetch(path, options)
  const data = await response.json()
  if (!response.ok) throw new Error(data.error || '请求失败')
  return data as T
}

export type Workflow = { id: number; name: string; description: string; enabled: boolean }
export type Task = { id: number; source_type: string; workflow_id: number; status: string; total_count: number; success_count: number; failed_count: number; created_at: string }
