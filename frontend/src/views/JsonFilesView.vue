<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'

type JsonFile = { id: number; filename: string; created_at: string }
type PageResult = { items: JsonFile[]; total: number; page: number; page_size: number }

const files = ref<JsonFile[]>([])
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const uploading = ref(false)
const loading = ref(false)
const viewing = ref<{ filename: string; content: string } | null>(null)

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    params.set('page', String(page.value))
    params.set('page_size', String(pageSize.value))
    const result = await api<PageResult>(`/api/json-files?${params}`)
    files.value = result.items
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

async function upload(event: Event) {
  const input = event.target as HTMLInputElement
  if (!input.files?.[0]) return
  uploading.value = true
  message.value = ''
  const body = new FormData()
  body.append('file', input.files[0])
  try {
    const data = await api<{ filename: string; total: number; inserted: number; skipped_duplicates: number; group_name: string }>('/api/json-files/upload', { method: 'POST', body })
    message.value = `导入成功：${data.inserted} 条入提示词库，跳过 ${data.skipped_duplicates} 条重复（分组「${data.group_name}」）`
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '上传失败'
    messageType.value = 'error'
  } finally {
    uploading.value = false
    input.value = ''
  }
}

async function viewFile(id: number) {
  try {
    const data = await api<{ filename: string; content: string }>(`/api/json-files/${id}`)
    viewing.value = data
  } catch (e) {
    alert(e instanceof Error ? e.message : '查看失败')
  }
}

async function deleteFile(id: number, name: string) {
  if (!confirm(`确定删除备份文件「${name}」？\n（已入库的提示词不受影响）`)) return
  try {
    await api(`/api/json-files/${id}`, { method: 'DELETE' })
    await load()
  } catch (e) {
    alert(e instanceof Error ? e.message : '删除失败')
  }
}

function formatTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

onMounted(load)
</script>

<template>
  <PageHeader eyebrow="PROMPT DATA" title="JSON 文件" description="上传提示词 JSON 自动入提示词库，源文件仅作备份。" />

  <div class="toolbar">
    <label class="upload-btn">
      <span v-if="uploading" class="spinner"></span>
      {{ uploading ? '导入中...' : '导入 JSON' }}
      <input type="file" accept=".json,application/json" @change="upload" />
    </label>
    <RouterLink to="/prompts" class="btn btn-ghost">前往提示词库 &rarr;</RouterLink>
    <span v-if="message" class="text-sm" :style="{ color: messageType === 'success' ? 'var(--c-success)' : 'var(--c-danger)' }">{{ message }}</span>
  </div>

  <div class="card">
    <div v-if="!files.length" class="empty-state">
      <div class="empty-icon">&#128196;</div>
      <p>暂无备份文件。上传 JSON 后，提示词会自动加入「提示词库」。</p>
    </div>
    <div v-else>
      <div v-for="file in files" :key="file.id" class="list-row">
        <div class="flex-1" style="min-width:0;">
          <div style="font-weight:600; margin-bottom:2px; word-break:break-all;">{{ file.filename }}</div>
          <div class="text-xs muted">{{ formatTime(file.created_at) }} 上传</div>
        </div>
        <a :href="`/api/json-files/${file.id}/download`" class="btn btn-ghost btn-sm" title="下载">&#8681;</a>
        <button class="btn btn-ghost btn-sm" @click="viewFile(file.id)" title="查看">&#128065;</button>
        <button class="btn btn-ghost btn-sm" @click="deleteFile(file.id, file.filename)" title="删除">&#10005;</button>
      </div>
    </div>
  </div>

  <div v-if="total > 0" class="pagination-footer">
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

  <!-- 查看弹窗 -->
  <div v-if="viewing" class="modal-overlay" @click.self="viewing = null">
    <div class="modal modal-wide">
      <h3 style="margin-top:0; word-break:break-all;">{{ viewing.filename }}</h3>
      <pre class="json-preview">{{ viewing.content }}</pre>
      <div style="display:flex; gap:8px; justify-content:flex-end; margin-top:16px;">
        <a :href="`/api/json-files/${files.find(f => f.filename === viewing.filename)?.id}/download`" class="btn btn-secondary">下载</a>
        <button class="btn btn-ghost" @click="viewing = null">关闭</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.5); display: flex; align-items: center; justify-content: center; z-index: 100; padding: 20px; }
.modal { background: var(--c-card); border-radius: 12px; padding: 24px; width: 100%; max-width: 520px; max-height: 85vh; overflow-y: auto; box-shadow: var(--shadow-lg); }
.modal-wide { max-width: 760px; }
.json-preview { background: #1e293b; color: #e2e8f0; padding: 16px; border-radius: 8px; font-size: 12px; line-height: 1.6; max-height: 55vh; overflow: auto; white-space: pre-wrap; word-break: break-word; }
</style>
