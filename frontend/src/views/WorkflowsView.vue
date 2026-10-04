<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import {
  api,
  type PromptCandidate,
  type PromptTarget,
  type PromptTargetsResult,
  type ValidationResult,
  type Workflow,
} from '../api/client'
import { useUrlState } from '../composables/useUrlState'

type PageResult = { items: Workflow[]; total: number; page: number; page_size: number }

/** 下拉框里「自动」的值。空串即自动，和后端「留空 = 自动识别」的语义对齐。 */
const AUTO = ''

const workflows = ref<Workflow[]>([])
const name = ref('')
const description = ref('')
const negativePrompt = ref('')
const workflowJSON = ref('')
/** 候选提示词节点，由后端按工作流结构算出来（不需要 ComfyUI 在线） */
const candidates = ref<PromptCandidate[]>([])
const targets = ref<PromptTargetsResult | null>(null)
const positiveChoice = ref(AUTO)
const negativeChoice = ref(AUTO)
/** 编辑已有工作流时带回来的、已经保存过的落点。优先于自动识别，避免手选被覆盖。 */
const storedMapping = ref<{ positive_prompt?: PromptTarget; negative_prompt?: PromptTarget }>({})

const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const busy = ref(false)
const loading = ref(false)
const showForm = ref(false)
const editingId = ref<number | null>(null)
const detecting = ref(false)
const validating = ref(false)
const validation = ref<ValidationResult | null>(null)
const defaultBusy = ref<number | null>(null)

/**
 * 报错的「节点 + 字段」索引。
 *
 * 能在提交时被自动纠正的报错（后端 repair.go：把串位的控件值挪回原位）会被后端
 * **同时**产出两条：一条 `value_not_in_list` 进 errors，一条 `auto_repairable` 进 warnings。
 * 两条说的是同一件事，只该呈现一次 —— 所以按这个键把 errors 里对应的那条摘掉。
 */
function issueKey(issue: { node_id: string; field?: string }) {
  return `${issue.node_id}\u0000${issue.field ?? ''}`
}

/** 自动纠正类警告（`auto_repairable`），单独一组。 */
const repairWarnings = computed(() =>
  (validation.value?.warnings ?? []).filter((w) => w.kind === 'auto_repairable'),
)

/** 其余警告（widget_values_mismatch 等），与自动纠正分开列。 */
const otherWarnings = computed(() =>
  (validation.value?.warnings ?? []).filter((w) => w.kind !== 'auto_repairable'),
)

const repairableKeys = computed(() => new Set(repairWarnings.value.map(issueKey)))

/**
 * 真正会失败、且**不会**被自动纠正的报错。
 * 可纠正的那些从 errors 里摘出来，改由下面的可纠正条目统一呈现，不再重复一遍。
 */
const visibleErrors = computed(() =>
  (validation.value?.errors ?? []).filter(
    (issue) => !(issue.kind === 'value_not_in_list' && repairableKeys.value.has(issueKey(issue))),
  ),
)

/**
 * 每条可纠正项带上原报错里的候选列表 —— 合并展示不该把「候选 + 点击复制」丢掉，
 * 用户看完仍然可以手动改成别的值。
 */
const repairItems = computed(() =>
  repairWarnings.value.map((warning) => {
    const key = issueKey(warning)
    const source = (validation.value?.errors ?? []).find(
      (issue) => issue.kind === 'value_not_in_list' && issueKey(issue) === key,
    )
    return { warning, candidates: source?.candidates ?? [], value: source?.value }
  }),
)

/** 提交时会被自动纠正的条数。 */
const autoFixableCount = computed(() => repairItems.value.length)

/**
 * 一份工作流的报错**全部**都能自动纠正时，标题不该写成「会导致提交失败」——
 * 它确实能提交成功，只是提交时会先被拒一次、再把值挪回原位。
 *
 * 判据必须看 visibleErrors（摘掉可纠正项之后）。只看 `errors` 的 kind 会漏判：
 * 若某字段的值不在候选里、又无处可挪（修不了），它同样是 `value_not_in_list`，
 * 却不会被自动纠正 —— 那时标题写成「会自动纠正」就把真失败藏起来了。
 */
const onlyAutoFixable = computed(() => {
  const result = validation.value
  if (!result || result.ok || result.unreachable) return false
  return visibleErrors.value.length === 0 && repairItems.value.length > 0
})

const total = ref(0)

// 分页进 URL query：离开再回来、刷新、分享链接都保持页码
const state = useUrlState({
  page: 1,
  page_size: 20,
}, {
  onExternalSync: load,
})

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/workflows?${params}`)
    workflows.value = result.items
    total.value = result.total
  } catch (e) {
    message.value = e instanceof Error ? e.message : '加载失败'
    messageType.value = 'error'
  } finally {
    loading.value = false
  }
}

function onPageChange(p: number) {
  state.page = p
  load()
}

function onPageSizeChange() {
  state.page = 1
  load()
}

// ---------------------------------------------------------------------------
// 提示词落点
// ---------------------------------------------------------------------------

function choiceKey(candidate: PromptCandidate) {
  return candidate.field ? `${candidate.node_id}|${candidate.field}` : candidate.node_id
}

function parseChoice(value: string): PromptTarget | null {
  if (!value) return null
  const [nodeID, field] = value.split('|')
  if (!nodeID) return null
  return { node_id: nodeID, field: field || '' }
}

function choiceOf(target?: PromptTarget | null) {
  if (!target || !target.node_id) return AUTO
  return target.field ? `${target.node_id}|${target.field}` : target.node_id
}

function choiceLabel(candidate: PromptCandidate) {
  const field = candidate.field ? `.${candidate.field}` : ''
  const preview = candidate.preview
    ? ` · ${candidate.preview.length > 24 ? candidate.preview.slice(0, 24) + '…' : candidate.preview}`
    : ''
  return `节点 ${candidate.node_id} ${candidate.class_type}${field}${preview}`
}

/** 已保存的落点优先，其次才是本次自动识别的结果。 */
function fillChoices() {
  const detected = targets.value?.detected
  const stored = storedMapping.value || {}
  positiveChoice.value = choiceOf(stored.positive_prompt ?? detected?.positive)
  negativeChoice.value = choiceOf(stored.negative_prompt ?? detected?.negative)
}

const detectedSummary = computed(() => {
  const detected = targets.value?.detected
  if (!detected) return '粘贴 Workflow JSON 后会自动识别正负向节点。'
  const describe = (target: PromptTarget | null) => {
    if (!target || !target.node_id) return '未识别'
    return `节点 ${target.node_id}${target.field ? '.' + target.field : ''}`
  }
  return `自动识别：正向 ${describe(detected.positive)}；负向 ${describe(detected.negative)}`
})

// 落点识别走的是不带 id 的通用入口，所以「还没保存的新工作流」也能先看结果。
async function detectTargets(fill = true) {
  if (!workflowJSON.value.trim()) {
    candidates.value = []
    targets.value = null
    return
  }
  detecting.value = true
  try {
    const data = await api<PromptTargetsResult>('/api/workflows/prompt-targets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workflow_json: JSON.parse(workflowJSON.value) }),
    })
    targets.value = data
    candidates.value = data.candidates || []
    if (fill) fillChoices()
  } catch {
    // JSON 还没写完或格式不对：保持原状，等下一次输入
  } finally {
    detecting.value = false
  }
}

// 换了 Workflow JSON 就等于换了节点：重新识别落点，并把下拉框刷成新结果。
let detectTimer: ReturnType<typeof setTimeout> | undefined
watch(workflowJSON, () => {
  clearTimeout(detectTimer)
  if (!workflowJSON.value.trim()) {
    candidates.value = []
    targets.value = null
    return
  }
  detectTimer = setTimeout(() => {
    // 新 JSON 会让旧的显式选择指向不存在的节点，所以这里不再沿用 stored
    storedMapping.value = {}
    detectTargets(true)
  }, 600)
})

function buildMapping() {
  const mapping: { positive_prompt?: PromptTarget; negative_prompt?: PromptTarget } = {}
  const positive = parseChoice(positiveChoice.value)
  const negative = parseChoice(negativeChoice.value)
  if (positive) mapping.positive_prompt = positive
  if (negative) mapping.negative_prompt = negative
  return mapping
}

// ---------------------------------------------------------------------------
// 提交前校验
// ---------------------------------------------------------------------------

// 把「提交给 ComfyUI 才知道合法与否」提前到编辑阶段。
// 定位是**提示**，不是闸门——不参与保存，也不参与提交。
async function validateCurrent() {
  if (!workflowJSON.value.trim()) {
    message.value = '请先粘贴 Workflow JSON'
    messageType.value = 'error'
    return
  }
  validating.value = true
  validation.value = null
  message.value = ''
  try {
    const result = await api<ValidationResult>('/api/workflows/validate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workflow_json: JSON.parse(workflowJSON.value) }),
    })
    validation.value = result
    if (result.unreachable) {
      // 连不上 ComfyUI 与工作流本身有问题要分开说，否则容易按错方向排查
      message.value = result.message || '无法连接 ComfyUI'
      messageType.value = 'error'
    } else if (result.ok) {
      message.value = '校验通过：节点与模型都在目标 ComfyUI 上'
      messageType.value = 'success'
    } else {
      message.value = `发现 ${result.errors.length} 个会导致提交失败的问题`
      messageType.value = 'error'
    }
  } catch (e) {
    message.value = e instanceof Error ? e.message : '校验失败，请检查 JSON 格式'
    messageType.value = 'error'
  } finally {
    validating.value = false
  }
}

// 校验时间戳转成本地可读格式；解析失败就原样显示，不要因此丢信息。
function formatCheckedAt(value: string) {
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString()
}

async function copyCandidate(value: string) {
  try {
    await navigator.clipboard.writeText(value)
    message.value = `已复制：${value}`
    messageType.value = 'success'
  } catch {
    // 剪贴板不可用（非 https / 无权限）时至少让人能手动选中
    message.value = value
    messageType.value = 'success'
  }
}

// ---------------------------------------------------------------------------
// 增删改
// ---------------------------------------------------------------------------

async function save() {
  busy.value = true
  message.value = ''
  try {
    // 未选的那一侧留空即可：后端会用自动识别的结果补上。
    const payload: any = {
      name: name.value,
      description: description.value,
      negative_prompt: negativePrompt.value,
      mapping: buildMapping(),
    }
    if (workflowJSON.value.trim()) payload.workflow_json = JSON.parse(workflowJSON.value)

    if (editingId.value) {
      await api(`/api/workflows/${editingId.value}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      })
      message.value = '工作流已更新'
    } else {
      if (!workflowJSON.value.trim()) throw new Error('请填写 Workflow JSON')
      await api('/api/workflows', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      })
      message.value = '工作流已创建'
    }
    messageType.value = 'success'
    resetForm()
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '保存失败'
    messageType.value = 'error'
  } finally {
    busy.value = false
  }
}

async function deleteWorkflow(wf: Workflow) {
  let tip = `确定删除工作流「${wf.name}」？`
  if ((wf.in_flight ?? 0) > 0) {
    tip = `该工作流还有 ${wf.in_flight} 个排队中/生成中的任务，需等待完成或先取消这些任务。\n\n仍要尝试删除吗？`
  } else if ((wf.task_count ?? 0) > 0) {
    tip = `该工作流有 ${wf.task_count} 条历史任务。删除后历史任务与图片记录保留，但无法再重试。\n\n确定删除工作流「${wf.name}」？`
  }
  if (!confirm(tip)) return
  try {
    await api(`/api/workflows/${wf.id}`, { method: 'DELETE' })
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

// 设为默认 / 取消默认：默认工作流全局唯一，由后端在事务内保证
async function setDefaultWorkflow(wf: Workflow, value: boolean) {
  defaultBusy.value = wf.id
  try {
    await api(`/api/workflows/${wf.id}/default`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ is_default: value }),
    })
    message.value = value ? `已将「${wf.name}」设为默认工作流` : '已取消默认工作流'
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '设置默认工作流失败'
    messageType.value = 'error'
  } finally {
    defaultBusy.value = null
  }
}

async function editWorkflow(wf: Workflow) {
  try {
    const detail = await api<Workflow>(`/api/workflows/${wf.id}`)
    // 先拿到已保存的落点，再灌 JSON —— watch 里的自动识别会用 stored 优先填充下拉框
    const resolved = await api<PromptTargetsResult>(`/api/workflows/${wf.id}/prompt-targets`)
    storedMapping.value = resolved.stored || {}
    targets.value = resolved
    candidates.value = resolved.candidates || []
    negativePrompt.value = detail.negative_prompt || ''
    editingId.value = wf.id
    name.value = wf.name
    description.value = wf.description || ''
    workflowJSON.value = JSON.stringify(detail.workflow_json, null, 2)
    fillChoices()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '读取工作流失败'
    messageType.value = 'error'
    return
  }
  showForm.value = true
  message.value = ''
  validation.value = null
}

function resetForm() {
  editingId.value = null
  name.value = ''
  description.value = ''
  negativePrompt.value = ''
  workflowJSON.value = ''
  candidates.value = []
  targets.value = null
  storedMapping.value = {}
  positiveChoice.value = AUTO
  negativeChoice.value = AUTO
  validation.value = null
  showForm.value = false
}

onMounted(load)
</script>

<template>
  <PageHeader eyebrow="WORKFLOWS" title="工作流" description="管理工作流，并指定正/负向提示词写到哪个节点。" />

  <!-- Create/Edit Form -->
  <div class="card mb-4" v-if="showForm" style="max-width:860px;">
    <div class="card-header">
      <h2>{{ editingId ? '编辑工作流' : '新增工作流' }}</h2>
      <button class="btn btn-ghost btn-sm" @click="resetForm">取消</button>
    </div>
    <div class="card-body">
      <div class="form-row">
        <div class="form-group">
          <label for="wf-name">名称</label>
          <input id="wf-name" v-model="name" type="text" placeholder="例如：普通人像" />
        </div>
        <div class="form-group">
          <label for="wf-desc">描述 <span class="text-xs muted">(可选)</span></label>
          <input id="wf-desc" v-model="description" type="text" placeholder="工作流用途说明" />
        </div>
      </div>

      <div class="form-group">
        <label for="wf-neg">通用负面提示词 <span class="text-xs muted">(该工作流默认)</span></label>
        <textarea id="wf-neg" v-model="negativePrompt" rows="3" placeholder="例如：nsfw, bad anatomy, worst quality..." spellcheck="false" />
      </div>

      <div class="form-group">
        <div style="display:flex; justify-content:space-between; align-items:center;">
          <label for="wf-json" style="margin:0;">Workflow JSON</label>
          <div style="display:flex; gap:8px;">
            <button class="btn btn-secondary btn-sm" :disabled="validating || detecting" @click="validateCurrent" type="button" title="把这份工作流拿给目标 ComfyUI 校验，提前发现会导致提交失败的字段">
              <span v-if="validating" class="spinner"></span>
              {{ validating ? '校验中...' : '校验工作流' }}
            </button>
          </div>
        </div>
        <textarea id="wf-json" v-model="workflowJSON" rows="8" placeholder='粘贴从 ComfyUI 导出的 workflow JSON...' spellcheck="false" style="margin-top:8px;" />
      </div>

      <!-- 提示词落点：本系统对工作流只做这一件事，其余内容原样提交给 ComfyUI。
           自动识别在不懂套路的结构上会失败（采样器不叫 positive/negative、
           单编码器结构等），所以这里必须能手选。 -->
      <div class="targets-panel">
        <div class="targets-title">
          提示词落点
          <span class="text-xs muted">(只指定正/负向写到哪；模型、尺寸、步数等一律按工作流 JSON 原样提交)</span>
        </div>
        <div class="form-row">
          <div class="form-group">
            <label class="text-xs" for="wf-positive-node">正向提示词节点</label>
            <select id="wf-positive-node" v-model="positiveChoice">
              <option :value="AUTO">自动识别</option>
              <option v-for="c in candidates" :key="`p-${choiceKey(c)}`" :value="choiceKey(c)">
                {{ choiceLabel(c) }}
              </option>
            </select>
          </div>
          <div class="form-group">
            <label class="text-xs" for="wf-negative-node">负向提示词节点</label>
            <select id="wf-negative-node" v-model="negativeChoice">
              <option :value="AUTO">自动识别</option>
              <option v-for="c in candidates" :key="`n-${choiceKey(c)}`" :value="choiceKey(c)">
                {{ choiceLabel(c) }}
              </option>
            </select>
          </div>
        </div>
        <p v-if="targets && targets.negative_unavailable" class="targets-hint is-warn">
          {{ targets.negative_unavailable }}
        </p>
        <p v-else class="targets-hint">
          {{ detecting ? '识别中...' : detectedSummary }}
        </p>
      </div>

      <!-- 校验结果：把「提交后才发现」的错提前到这里。
           定位是**提示**，不是闸门——它不参与保存，也不参与提交；
           候选值实时取自目标 ComfyUI 的节点定义，不是内置清单。 -->
      <div v-if="validation" class="validate-panel">
        <div class="validate-head">
          <strong v-if="validation.unreachable" style="color:var(--c-danger);">⚠ 无法连接 ComfyUI</strong>
          <strong v-else-if="validation.ok" style="color:var(--c-success);">✓ 校验通过</strong>
          <strong v-else-if="onlyAutoFixable" style="color:var(--c-warning, #b8860b);">
            ⚠ {{ autoFixableCount }} 处取值会在提交时自动纠正
          </strong>
          <strong v-else style="color:var(--c-danger);">✗ 发现 {{ visibleErrors.length }} 个会导致提交失败的问题</strong>
          <div style="display:flex; gap:4px;">
            <button class="btn btn-ghost btn-sm" type="button" :disabled="validating" @click="validateCurrent">重新校验</button>
            <button class="btn btn-ghost btn-sm" type="button" @click="validation = null">关闭</button>
          </div>
        </div>

        <p v-if="validation.unreachable" class="validate-line">{{ validation.message }}</p>
        <p v-else-if="validation.ok" class="validate-line">
          该工作流可以被 {{ validation.comfyui_url }} 接受：节点类型与模型文件都齐全。
        </p>

        <!-- 说明约束来自哪里：候选/节点/模型全是实时问 ComfyUI 要的，
             所以在 ComfyUI 上新增模型或节点后，这里会自动多出来，不需要改本系统 -->
        <p class="validate-line">
          候选值与节点定义实时取自
          <code>{{ validation.comfyui_url }}</code>
          （{{ formatCheckedAt(validation.checked_at) }}）—— 不是内置清单。
          在 ComfyUI 上装好新模型 / 新节点后重新校验，它们就会出现在候选里。
        </p>
        <p v-if="onlyAutoFixable" class="validate-line">
          提交时 ComfyUI 会先拒绝一次，服务把串位的值挪回原位后<strong>自动重试一次</strong>，
          所以这份工作流最终能跑通 —— 但节点值确实会被改掉（<strong>工作流文件本身不会被改动</strong>）。
        </p>
        <p v-else-if="!validation.ok && !validation.unreachable" class="validate-line">
          这只是提前告知：<strong>不影响保存，也不影响提交</strong>，值也可以手动改成任意内容。
        </p>

        <div v-for="(issue, idx) in visibleErrors" :key="`err-${idx}`" class="validate-item">
          <div class="validate-item-title">
            节点 {{ issue.node_id }} <span class="muted">({{ issue.class_type }})</span>
            <template v-if="issue.field"> · <code>{{ issue.field }}</code></template>
          </div>
          <div class="validate-item-msg">{{ issue.message }}</div>
          <div v-if="issue.candidates && issue.candidates.length" class="validate-cands">
            <span class="muted">候选（点击复制）：</span>
            <button
              v-for="cand in issue.candidates.slice(0, 10)"
              :key="cand"
              class="btn btn-ghost btn-sm cand-btn"
              type="button"
              @click="copyCandidate(cand)"
            >{{ cand }}</button>
            <span v-if="issue.candidates.length > 10" class="muted">…共 {{ issue.candidates.length }} 项</span>
          </div>
          <div v-if="issue.candidates && issue.candidates.length" class="muted text-xs" style="margin-top:6px;">
            这类问题不会被自动纠正，请把值改成候选里的某一项，否则提交会被 ComfyUI 拒绝。
          </div>
        </div>

        <!-- 提交时会被自动纠正的项：同一字段的那条红色报错已经在上面被摘掉了，
             这里只呈现一次，并把原报错里的候选列表带过来（用户仍可手动改成别的值）。 -->
        <div
          v-for="(item, idx) in repairItems"
          :key="`repair-${idx}`"
          class="validate-item is-warning"
        >
          <div class="validate-item-title">
            节点 {{ item.warning.node_id }} <span class="muted">({{ item.warning.class_type }})</span>
            <template v-if="item.warning.field"> · <code>{{ item.warning.field }}</code></template>
          </div>
          <div class="validate-item-msg">{{ item.warning.message }}</div>
          <div v-if="item.candidates.length" class="validate-cands">
            <span class="muted">候选（点击复制）：</span>
            <button
              v-for="cand in item.candidates.slice(0, 10)"
              :key="cand"
              class="btn btn-ghost btn-sm cand-btn"
              type="button"
              @click="copyCandidate(cand)"
            >{{ cand }}</button>
            <span v-if="item.candidates.length > 10" class="muted">…共 {{ item.candidates.length }} 项</span>
          </div>
        </div>

        <div v-for="(issue, idx) in otherWarnings" :key="`warn-${idx}`" class="validate-item is-warning">
          <div class="validate-item-title">
            节点 {{ issue.node_id }} <span class="muted">({{ issue.class_type }})</span>
            <template v-if="issue.field"> · <code>{{ issue.field }}</code></template>
          </div>
          <div class="validate-item-msg">{{ issue.message }}</div>
        </div>

        <!-- 被跳过的节点：与 ComfyUI 前端一致的行为（前端专用节点、旁路/静音节点
             不会进提交载荷）。报出来是为了让人知道「图里明明有这个节点，去哪了」，
             而不是在背后悄悄改写工作流 -->
        <div v-if="validation.skipped_nodes && validation.skipped_nodes.length" class="validate-item is-skipped">
          <div class="validate-item-title">
            {{ validation.skipped_nodes.length }} 个节点不会进入提交载荷
            <span class="muted">（与 ComfyUI 前端一致的行为）</span>
          </div>
          <div class="validate-item-msg">
            <span v-for="(node, idx) in validation.skipped_nodes" :key="`skip-${idx}`">
              <template v-if="idx">、</template>{{ node.node_id }} {{ node.class_type }}（{{ node.reason }}）
            </span>
          </div>
        </div>
      </div>

      <div class="btn-group">
        <button class="btn btn-primary" :disabled="busy || !name.trim()" @click="save">
          <span v-if="busy" class="spinner"></span>
          {{ busy ? '保存中...' : (editingId ? '更新工作流' : '创建工作流') }}
        </button>
        <button class="btn btn-secondary" @click="resetForm">取消</button>
      </div>
      <div v-if="message" class="status-msg mt-4" :class="messageType">{{ message }}</div>
    </div>
  </div>

  <!-- Workflow List -->
  <div class="toolbar" v-if="!showForm">
    <button class="btn btn-primary" @click="showForm = true">新增工作流</button>
    <span v-if="message" class="text-sm" :style="{ color: messageType === 'success' ? 'var(--c-success)' : 'var(--c-danger)' }">{{ message }}</span>
  </div>

  <div class="card" v-if="!showForm">
    <div v-if="!workflows.length" class="empty-state">
      <div class="empty-icon">&#9881;</div>
      <p>暂无工作流，点击上方按钮创建。</p>
    </div>
    <div v-else>
      <div v-for="wf in workflows" :key="wf.id" class="list-row">
        <div class="flex-1" style="min-width:0;">
          <div style="font-weight:600;">
            {{ wf.name }}
            <span v-if="wf.is_default" class="badge badge-default">默认</span>
          </div>
          <div class="text-xs muted" style="margin-top:3px;">{{ wf.description || '未填写描述' }}</div>
        </div>
        <span class="badge" :class="wf.enabled ? 'badge-success' : 'badge-cancelled'">
          {{ wf.enabled ? '启用' : '禁用' }}
        </span>
        <button
          class="btn btn-ghost btn-sm"
          :class="{ 'is-default': wf.is_default }"
          :disabled="defaultBusy === wf.id"
          :title="wf.is_default ? '取消默认工作流' : '设为默认工作流'"
          @click="setDefaultWorkflow(wf, !wf.is_default)"
        >{{ wf.is_default ? '★ 默认' : '☆ 设为默认' }}</button>
        <button class="btn btn-ghost btn-sm" @click="editWorkflow(wf)" title="编辑">&#9998;</button>
        <button class="btn btn-ghost btn-sm" @click="deleteWorkflow(wf)" title="删除">&#10005;</button>
      </div>
    </div>
  </div>

  <div v-if="!showForm && total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="state.page"
      :page-size="state.page_size"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <select v-model.number="state.page_size" class="input page-size-select" @change="onPageSizeChange">
        <option :value="10">10</option>
        <option :value="20">20</option>
        <option :value="50">50</option>
        <option :value="100">100</option>
      </select>
      <span class="text-xs muted">条</span>
    </div>
  </div>
</template>

<style scoped>
.form-row { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
.badge-default { margin-left: 6px; background: var(--c-primary-light); color: var(--c-primary); border: 1px solid var(--c-primary-border); }
.is-default { color: var(--c-primary); font-weight: 600; }

/* 提示词落点 */
.targets-panel { background: var(--c-bg-subtle); border: 1px solid var(--c-border); border-radius: 10px; padding: 14px; margin: 16px 0; }
.targets-title { font-weight: 600; font-size: 14px; margin-bottom: 12px; }
.targets-hint { font-size: 12px; color: var(--c-muted); margin: 8px 0 0; }
.targets-hint.is-warn { color: var(--c-warning, #b8860b); }

/* 工作流校验结果 */
.validate-panel { border: 1px solid var(--c-border); border-radius: 10px; padding: 12px 14px; margin: 12px 0; background: var(--c-bg-subtle); }
.validate-head { display: flex; justify-content: space-between; align-items: center; font-size: 14px; gap: 8px; }
.validate-line { font-size: 13px; color: var(--c-muted); margin: 8px 0 0; word-break: break-all; }
.validate-item { margin-top: 10px; padding: 10px 12px; border-radius: 8px; background: var(--c-bg, #fff); border-left: 3px solid var(--c-danger); }
.validate-item.is-warning { border-left-color: var(--c-warning, #b8860b); }
.validate-item.is-skipped { border-left-color: var(--c-border); }
.validate-item-title { font-size: 13px; font-weight: 600; }
.validate-item-title code { font-size: 12px; background: var(--c-bg-subtle); padding: 1px 5px; border-radius: 4px; }
.validate-item-msg { font-size: 13px; margin-top: 4px; word-break: break-all; }
.validate-cands { margin-top: 8px; font-size: 12px; display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.cand-btn { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 11px; padding: 2px 6px; border: 1px solid var(--c-border); }

@media (max-width: 600px) {
  .form-row { grid-template-columns: 1fr; }
}
</style>
