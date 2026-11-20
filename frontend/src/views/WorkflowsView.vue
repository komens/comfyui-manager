<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { Delete, EditPen } from '@element-plus/icons-vue'
import { ElMessageBox } from 'element-plus'
import 'element-plus/es/components/message-box/style/css'
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
const loading = ref(true)
const showForm = ref(false)
const editingId = ref<number | null>(null)
const detecting = ref(false)
const validating = ref(false)
const validation = ref<ValidationResult | null>(null)
const workflowStatusBusy = ref(false)

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
const loadError = ref('')
let listRequest: AbortController | undefined
let searchTimer: ReturnType<typeof setTimeout> | undefined

const state = useUrlState({
  page: 1,
  page_size: 50,
  q: '',
  status: '',
}, {
  onExternalSync: () => {
    clearTimeout(searchTimer)
    void load()
  },
})

async function load() {
  listRequest?.abort()
  const request = new AbortController()
  listRequest = request
  loading.value = true
  loadError.value = ''
  try {
    const params = new URLSearchParams({
      page: String(state.page), page_size: String(state.page_size),
    })
    if (state.q.trim()) params.set('q', state.q.trim())
    if (state.status) params.set('status', state.status)
    const result = await api<PageResult>(`/api/workflows?${params}`, { signal: request.signal })
    if (request.signal.aborted) return
    const lastPage = Math.max(1, Math.ceil(result.total / state.page_size))
    if (state.page > lastPage) {
      state.page = lastPage
      await load()
      return
    }
    workflows.value = result.items
    total.value = result.total
  } catch (e) {
    if (!request.signal.aborted) loadError.value = e instanceof Error ? e.message : '无法加载工作流'
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}

function applyFilters() {
  clearTimeout(searchTimer)
  state.page = 1
  void load()
}

function scheduleSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(applyFilters, 250)
}

function clearFilters() {
  state.q = ''
  state.status = ''
  applyFilters()
}

onBeforeUnmount(() => {
  clearTimeout(searchTimer)
  listRequest?.abort()
})

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
  try {
    await ElMessageBox.confirm(tip, '删除工作流', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api(`/api/workflows/${wf.id}`, { method: 'DELETE' })
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

async function setWorkflowEnabled(wf: Workflow) {
  const enabled = !wf.enabled
  const action = enabled ? '启用' : '禁用'
  workflowStatusBusy.value = true
  try {
    const status = await api<Pick<Workflow, 'enabled' | 'is_default'>>(`/api/workflows/${wf.id}/enabled`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled }),
    })
    const row = workflows.value.find(item => item.id === wf.id)
    if (row) Object.assign(row, status)
    message.value = `已${action}「${wf.name}」`
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : `${action}工作流失败`
    messageType.value = 'error'
  } finally {
    workflowStatusBusy.value = false
  }
}

// 设为默认 / 取消默认：默认工作流全局唯一，由后端在事务内保证
async function setDefaultWorkflow(wf: Workflow, value: boolean) {
  workflowStatusBusy.value = true
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
    workflowStatusBusy.value = false
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
  <PageHeader title="工作流" description="管理工作流，并指定正/负向提示词写到哪个节点。" />

  <!-- Create/Edit Form -->
  <div class="card mb-4" v-if="showForm" style="max-width:860px;">
    <div class="card-header">
      <h2>{{ editingId ? '编辑工作流' : '新增工作流' }}</h2>
      <el-button size="small" text @click="resetForm">取消</el-button>
    </div>
    <div class="card-body">
      <div class="form-row">
        <div class="form-group">
          <label for="wf-name">名称</label>
          <el-input id="wf-name" v-model="name" type="text" placeholder="例如：普通人像" />
        </div>
        <div class="form-group">
          <label for="wf-desc">描述 <span class="text-xs muted">(可选)</span></label>
          <el-input id="wf-desc" v-model="description" type="text" placeholder="工作流用途说明" />
        </div>
      </div>

      <div class="form-group">
        <label for="wf-neg">通用负面提示词 <span class="text-xs muted">(该工作流默认)</span></label>
        <el-input id="wf-neg" type="textarea" v-model="negativePrompt" :rows="3" placeholder="例如：nsfw, bad anatomy, worst quality..." spellcheck="false" />
      </div>

      <div class="form-group">
        <div class="workflow-json-heading">
          <label for="wf-json" style="margin:0;">Workflow JSON</label>
          <el-button size="small" :loading="validating" :disabled="detecting" @click="validateCurrent">
            校验工作流
          </el-button>
        </div>
        <el-input id="wf-json" type="textarea" class="workflow-json" v-model="workflowJSON" :rows="8" placeholder='粘贴从 ComfyUI 导出的 workflow JSON...' spellcheck="false" style="margin-top:8px;" />
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
            <el-select id="wf-positive-node" v-model="positiveChoice" filterable :empty-values="[]" popper-class="workflow-node-options">
              <el-option label="自动识别" :value="AUTO" />
              <el-option v-for="c in candidates" :key="`p-${choiceKey(c)}`" :label="choiceLabel(c)" :value="choiceKey(c)" />
            </el-select>
          </div>
          <div class="form-group">
            <label class="text-xs" for="wf-negative-node">负向提示词节点</label>
            <el-select id="wf-negative-node" v-model="negativeChoice" filterable :empty-values="[]" popper-class="workflow-node-options">
              <el-option label="自动识别" :value="AUTO" />
              <el-option v-for="c in candidates" :key="`n-${choiceKey(c)}`" :label="choiceLabel(c)" :value="choiceKey(c)" />
            </el-select>
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
          <div class="workflow-actions">
            <el-button size="small" text :disabled="validating" @click="validateCurrent">重新校验</el-button>
            <el-button size="small" text @click="validation = null">关闭</el-button>
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
            <el-button
              v-for="cand in issue.candidates.slice(0, 10)"
              :key="cand"
              class="cand-btn" size="small"

              @click="copyCandidate(cand)"
            >{{ cand }}</el-button>
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
            <el-button
              v-for="cand in item.candidates.slice(0, 10)"
              :key="cand"
              class="cand-btn" size="small"

              @click="copyCandidate(cand)"
            >{{ cand }}</el-button>
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
        <el-button type="primary" :loading="busy" :disabled="!name.trim()" @click="save">
          {{ editingId ? '更新工作流' : '创建工作流' }}
        </el-button>
        <el-button @click="resetForm">取消</el-button>
      </div>
      <ViewFeedback :kind="messageType" :message="message" />
    </div>
  </div>

  <!-- Workflow List -->
  <div v-if="!showForm" class="toolbar">
    <el-input v-model="state.q" class="toolbar-search" placeholder="搜索工作流名称" aria-label="搜索工作流名称"
      clearable @input="scheduleSearch" @keydown.enter="applyFilters" />
    <el-select v-model="state.status" class="filter-select" aria-label="工作流状态" :empty-values="[]" @change="applyFilters">
      <el-option value="" label="全部工作流" />
      <el-option value="enabled" label="已启用" />
      <el-option value="disabled" label="已禁用" />
      <el-option value="default" label="默认工作流" />
    </el-select>
    <el-button v-if="state.q || state.status" text @click="clearFilters">清空筛选</el-button>
    <el-button type="primary" @click="showForm = true">新增工作流</el-button>
  </div>
  <ViewFeedback v-if="!showForm" :kind="messageType" :message="message" />

  <div class="card" v-if="!showForm">
    <ViewFeedback v-if="loading && !workflows.length" kind="loading" />
    <ViewFeedback v-else-if="loadError" kind="error" :message="loadError">
      <el-button text size="small" @click="load">重试</el-button>
    </ViewFeedback>
    <ViewFeedback v-else-if="!workflows.length" kind="empty" :message="state.q || state.status ? '没有匹配的工作流' : '暂无工作流'" />
    <ul v-else class="workflow-list">
      <li v-for="wf in workflows" :key="wf.id" class="list-row workflow-row">
        <div class="workflow-summary">
          <div class="workflow-name">{{ wf.name }}</div>
          <div class="workflow-tags">
            <el-tag v-if="wf.is_default" size="small">默认</el-tag>
            <el-tag :type="wf.enabled ? 'success' : 'info'" size="small">{{ wf.enabled ? '已启用' : '已禁用' }}</el-tag>
          </div>
          <div v-if="wf.description" class="text-xs muted workflow-description">{{ wf.description }}</div>
        </div>
        <div class="workflow-actions">
          <el-button size="small" :disabled="workflowStatusBusy" @click="setWorkflowEnabled(wf)">
            {{ wf.enabled ? '禁用' : '启用' }}
          </el-button>
          <el-button v-if="wf.enabled" size="small" :disabled="workflowStatusBusy" @click="setDefaultWorkflow(wf, !wf.is_default)">
            {{ wf.is_default ? '取消默认' : '设为默认' }}
          </el-button>
          <el-button size="small" :icon="EditPen" @click="editWorkflow(wf)">编辑</el-button>
          <el-button size="small" type="danger" plain :icon="Delete" @click="deleteWorkflow(wf)">删除</el-button>
        </div>
      </li>
    </ul>
  </div>

  <div v-if="!showForm && !loadError && total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="state.page"
      :page-size="state.page_size"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <el-select v-model="state.page_size" class="page-size-select" size="small" aria-label="每页条数" @change="onPageSizeChange">
        <el-option v-for="size in [20, 50, 100, 200]" :key="size" :label="String(size)" :value="size" />
      </el-select>
      <span class="text-xs muted">条</span>
    </div>
  </div>
</template>

<style scoped>
.form-row { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.workflow-list { margin: 0; padding: 0; list-style: none; }
.workflow-summary { flex: 1; min-width: 0; }
.workflow-name { font-weight: 600; overflow-wrap: anywhere; }
.workflow-tags, .workflow-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
.workflow-tags, .workflow-description { margin-top: 5px; }
.workflow-description { overflow-wrap: anywhere; }
.workflow-actions { justify-content: flex-end; }
.workflow-actions .el-button + .el-button, .validate-cands .el-button + .el-button { margin-left: 0; }
.workflow-json-heading { display: flex; align-items: center; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
.workflow-json :deep(textarea) { min-width: 0; font-family: ui-monospace, monospace; }
:global(.workflow-node-options) { max-width: calc(100vw - 24px); }
.workflow-row .workflow-actions { flex: 0 0 auto; }
.page-size-select { width: 76px; }

/* 提示词落点 */
.targets-panel { background: var(--c-bg-subtle); border: 1px solid var(--c-border); border-radius: 10px; padding: 14px; margin: 16px 0; }
.targets-title { font-weight: 600; font-size: 14px; margin-bottom: 12px; }
.targets-hint { font-size: 12px; color: var(--c-muted); margin: 8px 0 0; }
.targets-hint.is-warn { color: var(--c-warning, #b8860b); }

/* 工作流校验结果 */
.validate-panel { border: 1px solid var(--c-border); border-radius: 10px; padding: 12px 14px; margin: 12px 0; background: var(--c-bg-subtle); }
.validate-head { display: flex; justify-content: space-between; align-items: center; font-size: 14px; gap: 8px; flex-wrap: wrap; }
.validate-line { font-size: 13px; color: var(--c-muted); margin: 8px 0 0; word-break: break-all; }
.validate-item { margin-top: 10px; padding: 10px 12px; border-radius: 8px; background: var(--c-bg, #fff); border-left: 3px solid var(--c-danger); }
.validate-item.is-warning { border-left-color: var(--c-warning, #b8860b); }
.validate-item.is-skipped { border-left-color: var(--c-border); }
.validate-item-title { font-size: 13px; font-weight: 600; }
.validate-item-title code { font-size: 12px; background: var(--c-bg-subtle); padding: 1px 5px; border-radius: 4px; }
.validate-item-msg { font-size: 13px; margin-top: 4px; word-break: break-all; }
.validate-cands { margin-top: 8px; font-size: 12px; display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.cand-btn { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 11px; padding: 2px 6px; border: 1px solid var(--c-border); max-width: 100%; height: auto; min-height: 24px; white-space: normal; overflow-wrap: anywhere; }

@media (max-width: 768px) {
  .form-row { grid-template-columns: minmax(0, 1fr); gap: 0; }
  .workflow-row { flex-direction: column; align-items: stretch; }
  .workflow-row .workflow-actions { justify-content: flex-start; }
  .workflow-row .el-button { min-height: 36px; }
}
</style>
