<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { Delete, View } from '@element-plus/icons-vue'
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api, type Workflow } from '../api/client'
import { useUrlState } from '../composables/useUrlState'
import { showTaskSubmissionMessage } from '../utils/taskSubmissionMessage'
import { fetchWorkflows, pickWorkflowId, rememberWorkflowId } from '../utils/workflowParams'

type JsonFile = { id: number; filename: string; created_at: string }
type PageResult = { items: JsonFile[]; total: number; page: number; page_size: number }
type ImportResult = {
  filename: string
  inserted: number
  skipped_duplicates: number
  group_name: string
  inserted_prompt_ids: number[]
}

const router = useRouter()
const files = ref<JsonFile[]>([])
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const uploading = ref(false)
const loading = ref(true)
const loadError = ref('')
const viewing = ref<{ id: number; filename: string; content: string } | null>(null)

// 粘贴 JSON 导入
const showTextImport = ref(false)
const textContent = ref('')
const textFilename = ref('')
const importingText = ref(false)
const importing = computed(() => uploading.value || importingText.value)

const importResult = ref<ImportResult | null>(null)
const submittedCount = ref(0)
const showSubmit = ref(false)
const workflows = ref<Workflow[]>([])
const workflowId = ref(0)
const loadingWorkflows = ref(false)
const workflowError = ref('')
const submitting = ref(false)
const submitError = ref('')
const importSummary = computed(() => {
  const result = importResult.value
  if (!result) return ''
  const added = result.inserted ? `新增 ${result.inserted} 条提示词` : '本次没有新增提示词'
  const submitted = submittedCount.value ? `，已提交 ${submittedCount.value} 个任务` : ''
  return `分组「${result.group_name}」${added}，跳过 ${result.skipped_duplicates} 条重复${submitted}`
})

const total = ref(0)

// 分页进 URL query：返回列表、刷新、分享链接都保持页码
const state = useUrlState({
  page: 1,
  page_size: 50,
}, {
  onExternalSync: load,
})

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const params = new URLSearchParams()
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/json-files?${params}`)
    files.value = result.items
    total.value = result.total
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : '无法加载 JSON 文件'
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

async function upload(event: Event) {
  const input = event.target as HTMLInputElement
  if (importing.value || !input.files?.[0]) return
  uploading.value = true
  message.value = ''
  const body = new FormData()
  body.append('file', input.files[0])
  try {
    const data = await api<ImportResult>('/api/json-files/upload', { method: 'POST', body })
    await finishImport(data)
  } catch (e) {
    message.value = e instanceof Error ? e.message : '上传失败'
    messageType.value = 'error'
  } finally {
    uploading.value = false
    input.value = ''
  }
}

async function importText() {
  if (importing.value) return
  const content = textContent.value.trim()
  if (!content) {
    message.value = '请先粘贴 JSON 内容'
    messageType.value = 'error'
    return
  }
  importingText.value = true
  message.value = ''
  try {
    const data = await api<ImportResult>('/api/json-files/import-text', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content, filename: textFilename.value.trim() || undefined }),
    })
    textContent.value = ''
    textFilename.value = ''
    showTextImport.value = false
    await finishImport(data)
  } catch (e) {
    message.value = e instanceof Error ? e.message : '导入失败'
    messageType.value = 'error'
  } finally {
    importingText.value = false
  }
}

async function finishImport(result: ImportResult) {
  importResult.value = result
  submittedCount.value = 0
  workflowId.value = 0
  await load()
}

async function loadWorkflows() {
  loadingWorkflows.value = true
  workflowError.value = ''
  try {
    workflows.value = (await fetchWorkflows()).filter(item => item.enabled)
    if (!workflows.value.some(item => item.id === workflowId.value)) {
      workflowId.value = pickWorkflowId(workflows.value)
    }
  } catch (e) {
    workflowError.value = e instanceof Error ? e.message : '无法加载工作流'
  } finally {
    loadingWorkflows.value = false
  }
}

function openSubmit() {
  submitError.value = ''
  showSubmit.value = true
  void loadWorkflows()
}

async function submitImported() {
  const ids = importResult.value?.inserted_prompt_ids
  const selectedWorkflow = workflowId.value
  if (submitting.value || submittedCount.value || !ids?.length || !selectedWorkflow) return

  submitting.value = true
  submitError.value = ''
  try {
    const result = await api<{ created: number }>('/api/prompts/batch-run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, workflow_id: selectedWorkflow }),
    })
    if (!result.created) {
      submitError.value = '本次导入的提示词已不可用，未创建任务。'
      return
    }
    submittedCount.value = result.created
    rememberWorkflowId(selectedWorkflow)
    showSubmit.value = false
    showTaskSubmissionMessage(router, `已提交 ${result.created} 个任务`, '查看任务列表', '/tasks')
  } catch (e) {
    submitError.value = e instanceof Error ? e.message : '提交作图失败'
  } finally {
    submitting.value = false
  }
}

async function viewFile(id: number) {
  try {
    const data = await api<{ filename: string; content: string }>(`/api/json-files/${id}`)
    viewing.value = { ...data, id }
  } catch (e) {
    message.value = e instanceof Error ? e.message : '查看失败'
    messageType.value = 'error'
  }
}

async function deleteFile(id: number, name: string) {
  if (!confirm(`确定删除备份文件「${name}」？\n（已入库的提示词不受影响）`)) return
  try {
    await api(`/api/json-files/${id}`, { method: 'DELETE' })
    message.value = '已删除备份文件'
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

function formatTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

onMounted(load)
</script>

<template>
  <PageHeader title="JSON 文件" description="上传提示词 JSON 自动入提示词库，源文件仅作备份。" />

  <div class="toolbar">
    <label class="upload-btn">
      <span v-if="uploading" class="spinner"></span>
      {{ uploading ? '导入中...' : '导入 JSON 文件' }}
      <input type="file" accept=".json,application/json" :disabled="importing" @change="upload" />
    </label>
    <el-button @click="showTextImport = !showTextImport">
      {{ showTextImport ? '收起粘贴导入' : '粘贴 JSON 导入' }}
    </el-button>
    <RouterLink to="/prompts" class="btn btn-ghost">前往提示词库 &rarr;</RouterLink>
  </div>
  <ViewFeedback :kind="messageType" :message="message" />
  <ViewFeedback v-if="importResult" kind="success" :message="importSummary">
    <el-button v-if="importResult.inserted_prompt_ids?.length && !submittedCount" type="primary" size="small"
      :disabled="importing" @click="openSubmit">提交作图</el-button>
  </ViewFeedback>

  <div v-if="showTextImport" class="card text-import-card">
    <div class="card-body">
      <div class="text-xs muted" style="margin-bottom:8px;">
        直接粘贴一段提示词 JSON（格式：数组，或 {"entries":[...]}，每条需含 positive 字段）。导入后会自动创建备份文件保存到项目 data/json/ 目录。
      </div>
      <el-input type="textarea"
        v-model="textContent"
        class="input json-textarea"
        :rows="8"
        placeholder='[{"title": "穿汉服的女孩", "positive": "a girl in hanfu, master piece"}]' />
      <div class="text-import-actions">
        <el-input
          v-model="textFilename"
          class="input filename-input"
          placeholder="文件名（可选，默认：手动导入-年月日时分秒.json）" />
        <el-button type="primary" :disabled="importing" @click="importText">
          {{ importingText ? '导入中...' : '导入' }}
        </el-button>
      </div>
    </div>
  </div>

  <div class="card">
    <ViewFeedback v-if="loading" kind="loading" />
    <ViewFeedback v-else-if="loadError" kind="error" :message="loadError">
      <el-button text size="small" @click="load">重试</el-button>
    </ViewFeedback>
    <ViewFeedback v-else-if="!files.length" kind="empty" message="暂无备份文件，上传 JSON 后提示词会加入提示词库" />
    <div v-else>
      <div v-for="file in files" :key="file.id" class="list-row">
        <div class="flex-1" style="min-width:0;">
          <div style="font-weight:600; margin-bottom:2px; word-break:break-all;">{{ file.filename }}</div>
          <div class="text-xs muted">{{ formatTime(file.created_at) }} 上传</div>
        </div>
        <a :href="`/api/json-files/${file.id}/download`" class="btn btn-ghost btn-sm" title="下载">&#8681;</a>
        <el-button text size="small" :icon="View" aria-label="查看" @click="viewFile(file.id)" />
        <el-button text type="danger" size="small" :icon="Delete" aria-label="删除" @click="deleteFile(file.id, file.filename)" />
      </div>
    </div>
  </div>

  <div v-if="total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="state.page"
      :page-size="state.page_size"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <el-select v-model="state.page_size" class="page-size-select" size="small" aria-label="每页条数" @change="onPageSizeChange">
        <el-option :value="20" label="20" />
        <el-option :value="50" label="50" />
        <el-option :value="100" label="100" />
        <el-option :value="200" label="200" />
      </el-select>
      <span class="text-xs muted">条</span>
    </div>
  </div>

  <el-dialog v-model="showSubmit" title="提交作图" class="app-dialog" width="min(460px, calc(100% - 24px))"
    top="10vh" append-to-body :show-close="!submitting" :close-on-click-modal="!submitting"
    :close-on-press-escape="!submitting">
    <div class="form-group">
      <label for="json-submit-workflow">为本次新增的 {{ importResult?.inserted }} 条提示词选择工作流</label>
      <el-select id="json-submit-workflow" v-model="workflowId" placeholder="请选择工作流" :empty-values="[0]"
        :disabled="loadingWorkflows || !!workflowError || !workflows.length || submitting" filterable popper-class="bounded-select-options">
        <el-option v-for="workflow in workflows" :key="workflow.id" :value="workflow.id" :label="workflow.name" />
      </el-select>
      <ViewFeedback v-if="loadingWorkflows" kind="loading" message="正在加载工作流" />
      <ViewFeedback v-else-if="workflowError" kind="error" :message="workflowError">
        <el-button text size="small" @click="loadWorkflows">重试</el-button>
      </ViewFeedback>
      <p v-else-if="!workflows.length" class="form-hint">
        请先创建或启用工作流。<RouterLink class="btn btn-secondary btn-sm" to="/workflows">管理工作流</RouterLink>
      </p>
    </div>
    <ViewFeedback kind="error" :message="submitError" />
    <template #footer>
      <el-button :disabled="submitting" @click="showSubmit = false">取消</el-button>
      <el-button type="primary" :loading="submitting" :disabled="!workflowId || loadingWorkflows || !!workflowError"
        @click="submitImported">确认提交</el-button>
    </template>
  </el-dialog>

  <el-dialog
    :model-value="!!viewing"
    :title="viewing?.filename"
    class="app-dialog"
    width="min(760px, calc(100% - 24px))"
    top="6vh"
    append-to-body
    @update:model-value="open => { if (!open) viewing = null }"
  >
    <pre v-if="viewing" class="json-preview">{{ viewing.content }}</pre>
    <template #footer>
      <a v-if="viewing" :href="`/api/json-files/${viewing.id}/download`" class="el-button">下载</a>
      <el-button @click="viewing = null">关闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.json-preview { background: #1e293b; color: #e2e8f0; padding: 16px; border-radius: 8px; font-size: 12px; line-height: 1.6; max-height: 55vh; overflow: auto; white-space: pre-wrap; word-break: break-word; }
.text-import-card { margin-bottom: 12px; }
.json-textarea :deep(textarea) { font-family: var(--font-mono, ui-monospace, monospace); font-size: 12.5px; resize: vertical; }
.text-import-actions { display: flex; gap: 8px; margin-top: 10px; flex-wrap: wrap; }
.filename-input { flex: 1 1 180px; min-width: 0; }
</style>
