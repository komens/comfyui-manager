import type { Workflow } from '../api/client'

export type ParamDef = {
  node_id: string
  field: string
  type?: string
  label?: string
  default?: any
  min?: number
  max?: number
  step?: number
  options?: string[]
  visible?: boolean
}

export type RunMapping = {
  positive_prompt?: { node_id: string; field: string }
  negative_prompt?: { node_id: string; field: string }
  seed?: { node_id: string; field: string }
  output_prefix?: { node_id: string; field: string }
  parameters?: Record<string, ParamDef>
} | null

/**
 * 合并 mapping.parameters（节点位置）与 params_schema（类型/标签/默认值）。
 *
 * mapping.parameters 只描述“写到哪个节点的哪个字段”，params_schema 才带 type。
 * `ParamForm` 靠 def.type 选择控件，因此缺 type 时 steps/cfg/width 会退化成文本输入框。
 * 这里做一次补全，让旧工作流（mapping 里没存 type）也能正确渲染。
 *
 * 注意：只补全 mapping.parameters 里已存在的键，不新增。schema 里的 seed 由后端
 * mapping.seed 单独处理，若在这里注入表单会被默认值覆盖掉随机种子。
 */
export function buildRunMapping(workflow?: Workflow | null): RunMapping {
  if (!workflow) return null
  const mapping = (workflow.mapping as RunMapping) || null
  if (!mapping) return null
  const schema = (workflow.params_schema as ParamDef[] | undefined) || []
  if (!schema.length || !mapping.parameters) return mapping

  const enhanced: Record<string, ParamDef> = { ...mapping.parameters }
  for (const p of schema) {
    const current = p?.name ? enhanced[p.name] : undefined
    if (!current) continue
    enhanced[p.name] = {
      ...current,
      type: p.type ?? current.type,
      label: p.label ?? current.label,
      default: p.default ?? current.default,
    }
  }
  return { ...mapping, parameters: enhanced }
}

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
