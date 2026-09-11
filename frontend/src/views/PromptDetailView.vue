<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import { useRoute, useRouter, RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import ParamForm from '../components/ParamForm.vue'
import { api, type Workflow } from '../api/client'
import { buildRunMapping, pickDefaultWorkflow } from '../utils/workflowParams'

type Run = {
  item_id: number
  task_id: number
  status: string
  comfy_prompt_id: string
  error_message: string
  workflow_id: number
  workflow_name: string
  images: { id: number; filename: string }[]
}
type PromptDetail = {
  id: number
  title: string
  description: string
  positive_prompt: string
  group_name: string
  status: string
  created_at: string
  runs: Run[]
}

const route = useRoute()
const router = useRouter()
const prompt = ref<PromptDetail | null>(null)
const loading = ref(true)
const workflows = ref<Workflow[]>([])
const showRunModal = ref(false)
const selectedWorkflow = ref<number>(0)
const params = ref<Record<string, any>>({})
const running = ref(false)
const lightboxImage = ref<number | null>(null)
const loadingError = ref('')
const message = ref('')
const messageType = ref<'success' | 'error'>('success')

function showMessage(text: string, type: 'success' | 'error' = 'success') {
  message.value = text
  messageType.value = type
  setTimeout(() => { message.value = '' }, 5000)
}

async function load() {
  loading.value = true
  loadingError.value = ''
  try {
    prompt.value = await api<PromptDetail>(`/api/prompts/${route.params.id}`)
  } catch (e) {
    loadingError.value = e instanceof Error ? e.message : '加载失败'
  } finally {
    loading.value = false
  }
}
async function loadWorkflows() {
  const result = await api<{ items: Workflow[] }>('/api/workflows?page_size=200')
  workflows.value = result.items
}

function openRunModal() {
  const enabled = workflows.value.filter(w => w.enabled)
  if (enabled.length) selectedWorkflow.value = enabled[0].id
  showRunModal.value = true
}

function onWorkflowChange() {
  params.value = {}
}

// 合并 mapping.parameters（节点位置）和 params_schema（类型/标签）
const runMapping = computed(() => buildRunMapping(workflows.value.find(w => w.id === selectedWorkflow.value)))
// 生成参数项数：折叠面板的标题提示与显隐判断
const runParamCount = computed(() => Object.keys(runMapping.value?.parameters ?? {}).length)

async function confirmRun() {
  if (!selectedWorkflow.value) { showMessage('请选择工作流', 'error'); return }
  running.value = true
  try {
    const result = await api<{ task_id: number }>(`/api/prompts/${route.params.id}/run`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workflow_id: selectedWorkflow.value, parameters: params.value }),
    })
    showRunModal.value = false
    router.push(`/tasks/${result.task_id}`)
  } catch (e) {
    showMessage(e instanceof Error ? e.message : '重跑失败', 'error')
  } finally {
    running.value = false
  }
}

function statusBadge(s: string) {
  const map: Record<string, string> = { pending: '待处理', running: '生成中', success: '成功', failed: '失败', queued: '排队中' }
  return map[s] || s
}

async function deletePrompt() {
  if (!confirm('确定删除该提示词？')) return
  await api(`/api/prompts/${route.params.id}`, { method: 'DELETE' })
  router.push('/prompts')
}

onMounted(() => {
  load()
  loadWorkflows()
})
</script>

<template>
  <PageHeader eyebrow="PROMPT DETAIL" title="提示词详情" description="查看提示词内容和历史生成结果。" />

  <div v-if="loading" class="card">
    <div class="card-body flex items-center gap-3">
      <span class="spinner"></span>
      <span class="muted">加载中...</span>
    </div>
  </div>

  <div v-else-if="loadingError" class="empty-state">
    <div class="empty-icon">&#9888;</div>
    <div>{{ loadingError }}</div>
    <button class="btn" style="margin-top: 12px" @click="load">重试</button>
  </div>

  <div v-else-if="prompt">
    <RouterLink to="/prompts" class="btn btn-ghost btn-sm" style="margin-bottom:12px;">&larr; 返回列表</RouterLink>

    <div class="card detail-card">
      <div class="card-body">
        <div class="detail-head">
          <div class="detail-title-wrap">
            <h2 class="detail-title">{{ prompt.title }}</h2>
            <div class="detail-badges">
              <span class="badge" :class="'badge-' + prompt.status">{{ statusBadge(prompt.status) }}</span>
              <span v-if="prompt.group_name" class="badge badge-muted">{{ prompt.group_name }}</span>
            </div>
          </div>
          <div class="detail-actions">
            <button class="btn btn-primary" @click="openRunModal">&#9654; 重跑</button>
            <RouterLink :to="`/prompts/${prompt.id}/edit`" class="btn btn-secondary">编辑</RouterLink>
            <button class="btn btn-ghost btn-danger" @click="deletePrompt">删除</button>
          </div>
        </div>
        <div v-if="prompt.description" class="text-sm muted detail-desc">{{ prompt.description }}</div>
        <div class="prompt-box">{{ prompt.positive_prompt }}</div>
      </div>
    </div>

    <h3 style="margin:20px 0 12px;">生成历史（{{ prompt.runs.length }}）</h3>
    <div v-if="!prompt.runs.length" class="card empty-state">
      <div class="empty-icon">&#128247;</div>
      <p>还没有生成记录，点击「重跑」开始生成。</p>
    </div>
    <div v-else class="run-list">
      <div v-for="run in prompt.runs" :key="run.item_id" class="card run-item">
        <div class="run-head">
          <span class="badge" :class="'badge-' + (run.status === 'success' ? 'done' : run.status)">{{ statusBadge(run.status) }}</span>
          <span class="text-sm">{{ run.workflow_name }}</span>
          <RouterLink :to="`/tasks/${run.task_id}`" class="text-sm link">任务 #{{ run.task_id }}</RouterLink>
        </div>
        <div v-if="run.error_message" class="text-sm" style="color:var(--c-danger); margin-top:6px;">{{ run.error_message }}</div>
        <div v-if="run.images.length" class="run-images">
          <img
            v-for="img in run.images"
            :key="img.id"
            :src="`/api/images/${img.id}/file`"
            :alt="img.filename"
            class="run-thumb"
            @click="lightboxImage = img.id"
          />
        </div>
      </div>
    </div>
  </div>

  <!-- 重跑弹窗 -->
  <div v-if="showRunModal" class="modal-overlay" @click.self="showRunModal = false">
    <div class="modal">
      <h3 style="margin-top:0;">重跑提示词</h3>
      <label class="form-label">选择工作流</label>
      <select v-model.number="selectedWorkflow" class="input" @change="onWorkflowChange">
        <option :value="0" disabled>请选择工作流</option>
        <option v-for="w in workflows.filter(x => x.enabled)" :key="w.id" :value="w.id">{{ w.name }}</option>
      </select>
      <!-- 生成参数：重跑时通常沿用工作流默认，属低频调整项，默认折叠。
           注意用 <details> 只是视觉隐藏，ParamForm 仍会挂载，
           默认值照常参与提交，不影响作图结果 -->
      <details v-if="selectedWorkflow && runParamCount > 0" class="collapse-panel">
        <summary>
          <span>生成参数</span>
          <span class="text-xs muted">{{ runParamCount }} 项 · 默认折叠</span>
        </summary>
        <div class="collapse-body">
          <ParamForm :mapping="runMapping" v-model="params" />
        </div>
      </details>
      <div style="display:flex; gap:8px; justify-content:flex-end; margin-top:16px;">
        <button class="btn btn-ghost" @click="showRunModal = false">取消</button>
        <button class="btn btn-primary" :disabled="running" @click="confirmRun">
          {{ running ? '提交中...' : '开始生成' }}
        </button>
      </div>
    </div>
  </div>

  <!-- 图片灯箱 -->
  <div v-if="lightboxImage" class="lightbox" @click.self="lightboxImage = null">
    <img :src="`/api/images/${lightboxImage}/file`" alt="preview" />
  </div>

  <!-- Toast 消息 -->
  <Teleport to="body">
    <div v-if="message" class="toast-msg" :class="messageType">{{ message }}</div>
  </Teleport>
</template>

<style scoped>
.detail-card { margin-bottom: 16px; }
.detail-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; margin-bottom: 12px; }
.detail-title-wrap { min-width: 0; flex: 1; }
.detail-title { margin: 0 0 8px; font-size: 22px; line-height: 1.3; word-break: break-word; }
.detail-badges { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.detail-actions { display: flex; gap: 8px; flex-shrink: 0; flex-wrap: wrap; justify-content: flex-end; }
.detail-desc { margin-bottom: 12px; }
.prompt-box { background: var(--c-bg-subtle); border: 1px solid var(--c-border); border-radius: 8px; padding: 14px; font-size: 14px; line-height: 1.7; white-space: pre-wrap; word-break: break-word; }
.btn-danger { color: var(--c-danger); }
.run-item { padding: 14px; }
.run-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.run-images { display: flex; gap: 10px; margin-top: 12px; flex-wrap: wrap; }
.run-thumb { width: 140px; height: 140px; object-fit: cover; border-radius: 8px; border: 1px solid var(--c-border); cursor: zoom-in; transition: transform 0.15s; }
.run-thumb:hover { transform: scale(1.03); }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
.modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.5); display: flex; align-items: center; justify-content: center; z-index: 100; padding: 20px; }
.modal { background: var(--c-card); border-radius: 12px; padding: 24px; width: 100%; max-width: 520px; max-height: 85vh; overflow-y: auto; box-shadow: var(--shadow-lg); }
.modal .collapse-panel { margin-top: 16px; }
.lightbox { position: fixed; inset: 0; background: rgba(0,0,0,0.85); display: flex; align-items: center; justify-content: center; z-index: 200; padding: 24px; cursor: zoom-out; }
.lightbox img { max-width: 90vw; max-height: 90vh; border-radius: 8px; }
@media (max-width: 640px) {
  .detail-head { flex-direction: column; gap: 12px; }
  .detail-actions { width: 100%; justify-content: stretch; }
  .detail-actions .btn { flex: 1; text-align: center; }
  .detail-title { font-size: 19px; }
}
.toast-msg {
  position: fixed;
  top: 20px;
  right: 20px;
  z-index: 300;
  padding: 10px 18px;
  border-radius: var(--radius-sm);
  font-size: 14px;
  font-weight: 500;
  box-shadow: var(--shadow-lg);
  animation: fadeIn .2s ease;
}
.toast-msg.success { background: var(--c-success-light); color: var(--c-success); border: 1px solid var(--c-success-border); }
.toast-msg.error { background: var(--c-danger-light); color: var(--c-danger); border: 1px solid var(--c-danger-border); }
@keyframes fadeIn { from { opacity: 0; transform: translateY(-8px); } to { opacity: 1; transform: translateY(0); } }
</style>
