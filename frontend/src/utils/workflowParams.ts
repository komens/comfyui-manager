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

/** 上次提交/重跑选中的工作流，三处入口共用一份记忆。 */
const LAST_WORKFLOW_KEY = 'last_workflow_id'

function readLastWorkflowId(): number {
  try {
    const id = Number(localStorage.getItem(LAST_WORKFLOW_KEY) || 0)
    return Number.isFinite(id) && id > 0 ? id : 0
  } catch {
    // 隐私模式下 localStorage 会抛异常，静默降级成「没记忆」
    return 0
  }
}

/** 记住本次选择；只在真正提交成功后调用，避免随手点下拉就覆盖。 */
export function rememberWorkflowId(id: number): void {
  if (!id) return
  try {
    localStorage.setItem(LAST_WORKFLOW_KEY, String(id))
  } catch {
    /* ignore */
  }
}

/**
 * 算出下拉框该选哪个工作流 id。
 * 优先上次用过的（仍存在，且 requireEnabled 时仍启用），否则按 pickDefaultWorkflow 兜底。
 * requireEnabled 用于「只列启用项」的重跑弹窗，避免选中一个不在选项里的值。
 */
export function pickWorkflowId(
  workflows: Workflow[] | undefined | null,
  options: { requireEnabled?: boolean } = {},
): number {
  if (!workflows || !workflows.length) return 0
  const usable = options.requireEnabled ? workflows.filter(w => w.enabled) : workflows
  if (!usable.length) return 0
  const last = readLastWorkflowId()
  const remembered = last ? usable.find(w => w.id === last) : undefined
  if (remembered) return remembered.id
  return pickDefaultWorkflow(usable)?.id ?? 0
}
