<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api } from '../api/client'

const url = ref('')
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('info')
const busy = ref(false)
const testBusy = ref(false)
const latency = ref<number | null>(null)
const exportFavorite = ref(false)
const exportBusy = ref(false)
const importBusy = ref(false)
const importResult = ref('')

onMounted(async () => {
  try {
    const data = await api<{ comfyui_url: string }>('/api/settings')
    url.value = data.comfyui_url
  } catch (e) {
    message.value = e instanceof Error ? e.message : '加载设置失败'
    messageType.value = 'error'
  }
})

async function save() {
  busy.value = true
  message.value = ''
  try {
    const data = await api<{ comfyui_url: string }>('/api/settings/comfyui', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: url.value }),
    })
    url.value = data.comfyui_url
    message.value = '地址已保存'
    messageType.value = 'success'
  } catch (e) {
    message.value = e instanceof Error ? e.message : '保存失败'
    messageType.value = 'error'
  } finally {
    busy.value = false
  }
}

function backupDB() {
  const a = document.createElement('a')
  a.href = '/api/settings/backup'
  a.download = ''
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
}

const fileInput = ref<HTMLInputElement | null>(null)

function triggerImport() {
  fileInput.value?.click()
}

async function exportData() {
  exportBusy.value = true
  try {
    const params = new URLSearchParams()
    if (exportFavorite.value) params.set('favorite', '1')
    const query = params.toString()
    const url = '/api/export' + (query ? '?' + query : '')
    const a = document.createElement('a')
    a.href = url
    a.download = ''
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  } finally {
    setTimeout(() => { exportBusy.value = false }, 1000)
  }
}

async function handleImport(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return

  importBusy.value = true
  importResult.value = ''
  try {
    const formData = new FormData()
    formData.append('file', file)
    const result = await api<{ imported_prompts: number; skipped_prompts: number; imported_images: number; skipped_images: number }>('/api/import', {
      method: 'POST',
      body: formData,
    })
    importResult.value = `导入成功！\n提示词：导入 ${result.imported_prompts} 条，跳过 ${result.skipped_prompts} 条重复\n图片：导入 ${result.imported_images} 张，跳过 ${result.skipped_images} 张重复`
  } catch (e) {
    importResult.value = '导入失败：' + (e instanceof Error ? e.message : '未知错误')
  } finally {
    importBusy.value = false
    input.value = ''  // 清空 file input
  }
}

async function test() {
  testBusy.value = true
  message.value = '正在测试连接...'
  messageType.value = 'info'
  latency.value = null
  try {
    const data = await api<{ latency_ms: number }>('/api/settings/comfyui/test?url=' + encodeURIComponent(url.value), { method: 'POST' })
    message.value = '连接成功'
    messageType.value = 'success'
    latency.value = data.latency_ms
  } catch (e) {
    message.value = e instanceof Error ? e.message : '连接失败'
    messageType.value = 'error'
  } finally {
    testBusy.value = false
  }
}
</script>

<template>
  <PageHeader eyebrow="SETTINGS" title="服务设置" description="配置 ComfyUI 服务地址和连接参数。" />

  <div class="card" style="max-width:640px;">
    <div class="card-header">
      <h2>ComfyUI 服务地址</h2>
    </div>
    <div class="card-body">
      <div class="form-group">
        <label for="url">服务地址</label>
        <input id="url" v-model="url" type="url" placeholder="http://192.168.1.20:8188" />
        <p class="form-hint">格式：http://IP:端口，内网 IP 变化后在此更新。</p>
      </div>

      <div class="btn-group">
        <button class="btn btn-primary" :disabled="busy" @click="save">
          <span v-if="busy" class="spinner"></span>
          {{ busy ? '保存中...' : '保存地址' }}
        </button>
        <button class="btn btn-secondary" :disabled="testBusy" @click="test">
          <span v-if="testBusy" class="spinner"></span>
          {{ testBusy ? '测试中...' : '测试连接' }}
        </button>
      </div>

      <div v-if="message" class="status-msg mt-4" :class="messageType">
        {{ message }}
        <span v-if="latency !== null"> &middot; 延迟 {{ latency }}ms</span>
      </div>
    </div>
  </div>

  <div class="card" style="margin-top: 20px; max-width:640px;">
    <div class="card-header">
      <h2>数据管理</h2>
    </div>
    <div class="card-body">
      <!-- 备份数据库 -->
      <div class="data-section">
        <h3>数据库备份</h3>
        <p class="muted">下载完整的 SQLite 数据库文件。</p>
        <button class="btn" @click="backupDB">&#8681; 备份数据库</button>
      </div>

      <div class="data-divider"></div>

      <!-- 导出数据 -->
      <div class="data-section">
        <h3>导出数据</h3>
        <p class="muted">按条件导出提示词和图片为 ZIP 包，可用于迁移到其他设备。</p>
        <div class="export-options">
          <label class="checkbox-label">
            <input type="checkbox" v-model="exportFavorite" />
            仅导出收藏
          </label>
        </div>
        <button class="btn btn-primary" :disabled="exportBusy" @click="exportData">
          <span v-if="exportBusy" class="spinner"></span>
          {{ exportBusy ? '导出中...' : '&#8681; 导出数据' }}
        </button>
      </div>

      <div class="data-divider"></div>

      <!-- 导入数据 -->
      <div class="data-section">
        <h3>导入数据</h3>
        <p class="muted">上传之前导出的 ZIP 包，自动合并去重。重复的提示词和图片会被跳过。</p>
        <div class="import-wrap">
          <input type="file" ref="fileInput" accept=".zip" style="display:none" @change="handleImport" />
          <button class="btn" :disabled="importBusy" @click="triggerImport">
            <span v-if="importBusy" class="spinner"></span>
            {{ importBusy ? '导入中...' : '&#8682; 选择文件导入' }}
          </button>
        </div>
        <div v-if="importResult" class="status-msg mt-4" :class="importResult.includes('成功') ? 'success' : 'info'" style="white-space: pre-line;">{{ importResult }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.data-section h3 {
  font-size: 14px;
  font-weight: 600;
  margin: 0 0 4px;
}
.data-section p {
  margin: 0 0 12px;
}
.data-divider {
  height: 1px;
  background: var(--c-border-light);
  margin: 20px 0;
}
.export-options {
  margin-bottom: 12px;
}
.checkbox-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 14px;
  cursor: pointer;
}
.checkbox-label input[type="checkbox"] {
  accent-color: var(--c-primary);
}
.import-wrap {
  display: flex;
  gap: 8px;
}
</style>
