import type { Workflow } from '../api/client'

/**
 * 选出下拉框应默认选中的工作流。
 * 优先「默认且启用」，其次第一个启用的，最后兜底第一个。
 * 用于直接提交、提示词重跑、批量重跑三处，避免各自写一套 if (items[0]) 逻辑。
 */
export function pickDefaultWorkflow(workflows: Workflow[] | undefined | null): Workflow | undefined {
  if (!workflows || !workflows.length) return undefined
  const enabled = workflows.filter(w => w.enabled)
  const pool = enabled.length ? enabled : workflows
  return pool.find(w => w.is_default) ?? pool[0]
}
