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
      <p class="muted" style="margin: 0 0 16px">备份数据库文件，包含所有工作流、提示词、任务记录和图片信息。</p>
      <button class="btn" @click="backupDB">
        &#8681; 备份数据库
      </button>
    </div>
  </div>
</template>
