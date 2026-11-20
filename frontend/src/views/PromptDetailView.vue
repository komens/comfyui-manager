<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { onMounted, onUnmounted, ref, computed, watch } from 'vue'
import { useRoute, useRouter, RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import PlatformBadge from '../components/PlatformBadge.vue'
import { api, type Workflow } from '../api/client'
import { thumbUrl } from '../utils/imageUrls'
import { fetchWorkflows, pickWorkflowId, rememberWorkflowId } from '../utils/workflowParams'
import { openImageViewer } from '../utils/imageViewer'
import { backToListOr } from '../utils/nav'
import { ensurePlatforms } from '../utils/platforms'

type Run = {
  item_id: number
  task_id: number
  status: string
  comfy_prompt_id: string
  error_message: string
  workflow_id: number
  workflow_name: string
  images: { id: number; filename: string; width?: number; height?: number }[]
}
type PromptImage = { id: number; filename: string; width?: number; height?: number }
type PromptDetail = {
  id: number
  title: string
  description: string
  positive_prompt: string
  negative_prompt: string
  group_name: string
  status: string
  is_favorite: boolean
  created_at: string
  source_platform: string
  // 外部同步的图不走生成项，后端单独返回一份
  external_images: PromptImage[]
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
const loadingError = ref('')
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
let pollTimer: ReturnType<typeof setInterval> | null = null

function showMessage(text: string, type: 'success' | 'error' = 'success') {
  message.value = text
  messageType.value = type
  setTimeout(() => { message.value = '' }, 5000)
}

async function load(silent = false) {
  if (!silent) {
    loading.value = true
    loadingError.value = ''
  }
  try {
    prompt.value = await api<PromptDetail>(`/api/prompts/${route.params.id}`)
  } catch (e) {
    if (!silent) loadingError.value = e instanceof Error ? e.message : '加载失败'
  } finally {
    if (!silent) loading.value = false
  }
}

// 有进行中的生成时轮询静默刷新；重跑弹窗打开时暂停，避免打断操作
const busy = computed(() => {
  const p = prompt.value
  if (!p) return false
  return p.status === 'running' || p.runs.some(r => ['pending', 'running', 'queued', 'submitted'].includes(r.status))
})
const paused = computed(() => showRunModal.value || running.value)

function setPolling(on: boolean) {
  if (on && !pollTimer) {
    pollTimer = setInterval(() => { if (!paused.value) load(true) }, 4000)
  } else if (!on && pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

watch(busy, v => setPolling(v), { immediate: true })
async function loadWorkflows() {
  workflows.value = await fetchWorkflows()
}

function openRunModal() {
  // 下拉只列启用项，所以记忆项被停用时自动退回默认
  selectedWorkflow.value = pickWorkflowId(workflows.value, { requireEnabled: true })
  showRunModal.value = true
}

function onWorkflowChange() {
  params.value = {}
}

async function confirmRun() {
  if (!selectedWorkflow.value) { showMessage('请选择工作流', 'error'); return }
  running.value = true
  try {
    const result = await api<{ task_id: number }>(`/api/prompts/${route.params.id}/run`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workflow_id: selectedWorkflow.value }),
    })
    showRunModal.value = false
    rememberWorkflowId(selectedWorkflow.value)
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
  backToList()
}

// 收藏以提示词为主体，后端会级联同步其名下所有结果图
async function toggleFavorite() {
  if (!prompt.value) return
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/prompts/${prompt.value.id}/favorite`, { method: 'PATCH' })
    prompt.value.is_favorite = result.is_favorite
    showMessage(result.is_favorite ? '已收藏（含关联图片）' : '已取消收藏（含关联图片）')
  } catch (e) {
    showMessage(e instanceof Error ? e.message : '操作失败', 'error')
  }
}

// 返回时优先走历史（保留列表页的分页/筛选 query），直链进入则回列表第一页
function backToList() {
  backToListOr(router, returnList.value)
}

const returnList = computed(() => {
  const previous = window.history.state?.back as string | undefined
  return previous?.split('?')[0] === '/gallery' ? '/gallery' : '/prompts'
})

function openRunImages(run: Run, index: number) {
  openImageViewer(run.images.map(i => ({ id: i.id, width: i.width, height: i.height })), index, {
    goToPrompt: id => router.push(`/prompts/${id}`),
    mutated: () => load(true),
  })
}

function openExternalImages(images: PromptImage[], index: number) {
  openImageViewer(images.map(i => ({ id: i.id, width: i.width, height: i.height })), index, {
    goToPrompt: id => router.push(`/prompts/${id}`),
    mutated: () => load(true),
  })
}

onMounted(() => {
  load()
  loadWorkflows()
  ensurePlatforms()
})
onUnmounted(() => setPolling(false))
</script>

<template>
  <PageHeader title="提示词详情" description="查看提示词内容和历史生成结果。" />

  <ViewFeedback :kind="messageType" :message="message" />
  <ViewFeedback v-if="loading" kind="loading" />
  <ViewFeedback v-else-if="loadingError" kind="error" :message="loadingError">
    <el-button text size="small" @click="load()">重试</el-button>
  </ViewFeedback>

  <div v-else-if="prompt">
    <el-button text size="small" style="margin-bottom:12px;" @click="backToList">&larr; {{ returnList === '/gallery' ? '返回图库' : '返回列表' }}</el-button>

    <div class="card detail-card">
      <div class="card-body">
        <div class="detail-head">
          <div class="detail-title-wrap">
            <h2 class="detail-title">{{ prompt.title }}</h2>
            <div class="detail-badges">
              <PlatformBadge v-if="prompt.source_platform" :platform="prompt.source_platform" size="md" />
              <span class="badge" :class="'badge-' + prompt.status">{{ statusBadge(prompt.status) }}</span>
              <span v-if="prompt.group_name" class="badge badge-muted">{{ prompt.group_name }}</span>
            </div>
          </div>
          <div class="detail-actions">
            <el-button text :class="{ 'fav-active': prompt.is_favorite }" @click="toggleFavorite">
              {{ prompt.is_favorite ? '★ 已收藏' : '☆ 收藏' }}
            </el-button>
            <el-button type="primary" @click="openRunModal">&#9654; 重跑</el-button>
            <RouterLink :to="`/prompts/${prompt.id}/edit`" class="btn btn-secondary">编辑</RouterLink>
            <el-button type="danger" text @click="deletePrompt">删除</el-button>
          </div>
        </div>
        <div v-if="prompt.description" class="text-sm muted detail-desc">{{ prompt.description }}</div>
        <div class="prompt-box">{{ prompt.positive_prompt }}</div>
        <template v-if="prompt.negative_prompt">
          <div class="prompt-sub-label">负面提示词</div>
          <div class="prompt-box prompt-box-neg">{{ prompt.negative_prompt }}</div>
        </template>
      </div>
    </div>

    <!-- 外部平台同步的图片：不挂在生成项上，单独一块展示 -->
    <div v-if="prompt.external_images && prompt.external_images.length" class="card run-item external-block">
      <div class="run-head">
        <PlatformBadge v-if="prompt.source_platform" :platform="prompt.source_platform" />
        <span class="text-sm">外部来源图片（{{ prompt.external_images.length }}）</span>
      </div>
      <div class="run-images">
        <img
          v-for="(img, idx) in prompt.external_images"
          :key="img.id"
          :src="thumbUrl(img.id)"
          :alt="img.filename"
          class="run-thumb"
          loading="lazy"
          decoding="async"
          @click="openExternalImages(prompt.external_images, idx)"
        />
      </div>
    </div>

    <h3 style="margin:20px 0 12px;">生成历史（{{ prompt.runs.length }}）</h3>
    <ViewFeedback v-if="!prompt.runs.length" kind="empty"
      :message="prompt.source_platform ? '暂无本平台生成记录' : '暂无生成记录，可点击重跑开始生成'" />
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
            v-for="(img, idx) in run.images"
            :key="img.id"
            :src="thumbUrl(img.id)"
            :alt="img.filename"
            class="run-thumb"
            loading="lazy"
            decoding="async"
            @click="openRunImages(run, idx)"
          />
        </div>
      </div>
    </div>
  </div>

  <el-dialog v-model="showRunModal" title="重跑提示词" class="app-dialog" width="min(520px, calc(100% - 24px))" top="6vh" append-to-body>
    <label class="form-label" for="rerun-workflow">选择工作流</label>
    <el-select id="rerun-workflow" v-model="selectedWorkflow" @change="onWorkflowChange" filterable popper-class="bounded-select-options">
      <el-option :value="0" disabled label="请选择工作流" />
      <el-option v-for="w in workflows.filter(x => x.enabled)" :key="w.id" :value="w.id" :label="w.name" />
    </el-select>
    <template #footer>
      <el-button @click="showRunModal = false">取消</el-button>
      <el-button type="primary" :loading="running" :disabled="!selectedWorkflow" @click="confirmRun">开始生成</el-button>
    </template>
  </el-dialog>

</template>

<style scoped>
.detail-card { margin-bottom: 16px; }
.detail-actions .fav-active { color: #d97706; border-color: #fbbf24; background: #fff7ed; }
.detail-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 16px; margin-bottom: 12px; }
.detail-title-wrap { min-width: 0; flex: 1; }
.detail-title { margin: 0 0 8px; font-size: 22px; line-height: 1.3; word-break: break-word; }
.detail-badges { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.detail-actions { display: flex; gap: 8px; flex-shrink: 0; flex-wrap: wrap; justify-content: flex-end; }
.detail-actions .el-button + .el-button { margin-left: 0; }
.detail-desc { margin-bottom: 12px; }
.prompt-box { background: var(--c-bg-subtle); border: 1px solid var(--c-border); border-radius: 8px; padding: 14px; font-size: 14px; line-height: 1.7; white-space: pre-wrap; word-break: break-word; }
.prompt-sub-label { margin: 14px 0 6px; font-size: 12px; font-weight: 600; color: var(--c-text-muted, #888); letter-spacing: .04em; }
.prompt-box-neg { color: var(--c-text-muted, #666); }
.run-item { padding: 14px; margin-bottom: 12px; }
.run-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.run-images { display: flex; gap: 10px; margin-top: 12px; flex-wrap: wrap; }
.run-thumb { width: 140px; height: 140px; object-fit: cover; border-radius: 8px; border: 1px solid var(--c-border); cursor: zoom-in; transition: transform 0.15s; }
.run-thumb:hover { transform: scale(1.03); }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
@media (max-width: 640px) {
  .detail-head { flex-direction: column; gap: 12px; }
  .detail-actions { width: 100%; justify-content: stretch; }
  .detail-actions .el-button { flex: 1; text-align: center; }
  .detail-title { font-size: 19px; }
}
@keyframes fadeIn { from { opacity: 0; transform: translateY(-8px); } to { opacity: 1; transform: translateY(0); } }
</style>
