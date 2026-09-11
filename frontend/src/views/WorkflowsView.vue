<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api, type Workflow } from '../api/client'

type ParamDef = { name: string; label: string; type: string; node_id: string; field: string; default: any }
type PageResult = { items: Workflow[]; total: number; page: number; page_size: number }

const workflows = ref<Workflow[]>([])
const name = ref('')
const description = ref('')
const negativePrompt = ref('')
const workflowJSON = ref('')
const mapping = ref('')
const detectedParams = ref<ParamDef[]>([])
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const busy = ref(false)
const loading = ref(false)
const showForm = ref(false)
const editingId = ref<number | null>(null)
const detecting = ref(false)

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    params.set('page', String(page.value))
    params.set('page_size', String(pageSize.value))
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
  page.value = p
  load()
}

function onPageSizeChange() {
  page.value = 1
  load()
}

// 自动识别工作流参数
async function detectParams() {
  if (!workflowJSON.value.trim()) {
    message.value = '请先粘贴 Workflow JSON'
    messageType.value = 'error'
    return
  }
  detecting.value = true
  message.value = ''
  try {
    const parsed = JSON.parse(workflowJSON.value)
    const data = await api<{ params: ParamDef[]; mapping: any; params_schema: any }>('/api/workflows/detect-params', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workflow_json: parsed }),
    })
    detectedParams.value = data.params
    mapping.value = JSON.stringify(data.mapping, null, 2)
    message.value = `识别到 ${data.params.length} 个可编辑参数${data.negative_node ? '' : '（未自动识别负面提示词节点，请在映射中手动配置）'}`
    messageType.value = 'success'
  } catch (e) {
    message.value = e instanceof Error ? e.message : '识别失败，请检查 JSON 格式'
    messageType.value = 'error'
  } finally {
    detecting.value = false
  }
}

// 更新工作流 JSON 中某个参数的默认值（可视化编辑直接写回 JSON）
function updateWorkflowParam(p: ParamDef, value: any) {
  try {
    const parsed = JSON.parse(workflowJSON.value)
    const node = parsed[p.node_id]
    if (!node?.inputs) return
    const parts = p.field.split('.') // inputs.steps
    if (parts.length === 2 && parts[0] === 'inputs') {
      node.inputs[parts[1]] = value
    }
    workflowJSON.value = JSON.stringify(parsed, null, 2)
  } catch {
    // ignore parse errors
  }
}

async function save() {
  busy.value = true
  message.value = ''
  try {
    const payload: any = {
      name: name.value,
      description: description.value,
      negative_prompt: negativePrompt.value,
    }
    if (workflowJSON.value.trim()) payload.workflow_json = JSON.parse(workflowJSON.value)
    if (mapping.value.trim()) payload.mapping = JSON.parse(mapping.value)
    if (detectedParams.value.length) payload.params_schema = detectedParams.value

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

async function deleteWorkflow(id: number, wfName: string) {
  if (!confirm(`确定删除工作流 "${wfName}"？`)) return
  try {
    await api(`/api/workflows/${id}`, { method: 'DELETE' })
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

async function editWorkflow(wf: Workflow) {
  try {
    const detail = await api<Workflow & { workflow_json: any; params_schema: any }>(`/api/workflows/${wf.id}`)
    workflowJSON.value = JSON.stringify(detail.workflow_json, null, 2)
    mapping.value = JSON.stringify(detail.mapping || {}, null, 2)
    negativePrompt.value = detail.negative_prompt || ''
    detectedParams.value = Array.isArray(detail.params_schema) ? detail.params_schema : []
  } catch (e) {
    message.value = e instanceof Error ? e.message : '读取工作流失败'
    messageType.value = 'error'
    return
  }
  editingId.value = wf.id
  name.value = wf.name
  description.value = wf.description || ''
  showForm.value = true
  message.value = ''
}

function resetForm() {
  editingId.value = null
  name.value = ''
  description.value = ''
  negativePrompt.value = ''
  workflowJSON.value = ''
  mapping.value = ''
  detectedParams.value = []
  showForm.value = false
}

onMounted(load)
</script>

<template>
  <PageHeader eyebrow="WORKFLOWS" title="工作流" description="管理 ComfyUI 工作流，自动识别尺寸、步数等参数。" />

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
          <button class="btn btn-secondary btn-sm" :disabled="detecting" @click="detectParams" type="button">
            <span v-if="detecting" class="spinner"></span>
            {{ detecting ? '识别中...' : '自动识别参数' }}
          </button>
        </div>
        <textarea id="wf-json" v-model="workflowJSON" rows="8" placeholder='粘贴从 ComfyUI 导出的 workflow JSON...' spellcheck="false" style="margin-top:8px;" />
      </div>

      <!-- 可视化参数编辑 -->
      <div v-if="detectedParams.length" class="params-panel">
        <div class="params-panel-title">可编辑参数 <span class="text-xs muted">(修改会同步到 Workflow JSON)</span></div>
        <div class="params-grid">
          <div v-for="p in detectedParams" :key="p.name" class="param-item">
            <label>{{ p.label }}</label>
            <div v-if="p.type === 'integer' || p.type === 'number' || p.type === 'seed'" style="display:flex; gap:6px;">
              <input
                type="number"
                :step="p.type === 'number' ? '0.1' : '1'"
                :value="p.default"
                @input="updateWorkflowParam(p, parseFloat(($event.target as HTMLInputElement).value) || 0)"
              />
            </div>
            <div v-else>
              <input type="text" :value="p.default" @input="updateWorkflowParam(p, ($event.target as HTMLInputElement).value)" />
            </div>
          </div>
        </div>
      </div>

      <details class="adv-details">
        <summary>高级：节点映射 JSON</summary>
        <div class="form-group" style="margin-top:10px;">
          <textarea v-model="mapping" rows="6" placeholder='{"positive_prompt": {"node_id": "4", "field": "inputs.text"}, ...}' spellcheck="false" />
        </div>
      </details>

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
          <div style="font-weight:600;">{{ wf.name }}</div>
          <div class="text-xs muted" style="margin-top:3px;">{{ wf.description || '未填写描述' }}</div>
        </div>
        <span class="badge" :class="wf.enabled ? 'badge-success' : 'badge-cancelled'">
          {{ wf.enabled ? '启用' : '禁用' }}
        </span>
        <button class="btn btn-ghost btn-sm" @click="editWorkflow(wf)" title="编辑">&#9998;</button>
        <button class="btn btn-ghost btn-sm" @click="deleteWorkflow(wf.id, wf.name)" title="删除">&#10005;</button>
      </div>
    </div>
  </div>

  <div v-if="!showForm && total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="page"
      :page-size="pageSize"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <select v-model.number="pageSize" class="input page-size-select" @change="onPageSizeChange">
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
.params-panel { background: var(--c-bg-subtle); border: 1px solid var(--c-border); border-radius: 10px; padding: 14px; margin: 16px 0; }
.params-panel-title { font-weight: 600; font-size: 14px; margin-bottom: 12px; }
.params-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 12px; }
.param-item label { display: block; font-size: 12px; font-weight: 600; margin-bottom: 4px; color: var(--c-text); }
.param-item input { width: 100%; }
.adv-details summary { cursor: pointer; font-size: 13px; font-weight: 600; color: var(--c-muted); padding: 8px 0; }
@media (max-width: 600px) {
  .form-row { grid-template-columns: 1fr; }
}
</style>
