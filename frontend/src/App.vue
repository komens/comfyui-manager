<script setup lang="ts">
import { onMounted, ref } from 'vue'

const comfyuiUrl = ref('')
const message = ref('正在读取配置...')
const saving = ref(false)

async function loadSettings() {
  const response = await fetch('/api/settings')
  if (!response.ok) throw new Error('读取配置失败')
  const data = await response.json()
  comfyuiUrl.value = data.comfyui_url
  message.value = '配置已加载'
}

async function saveSettings() {
  saving.value = true
  try {
    const response = await fetch('/api/settings/comfyui', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: comfyuiUrl.value }),
    })
    const data = await response.json()
    if (!response.ok) throw new Error(data.error || '保存失败')
    comfyuiUrl.value = data.comfyui_url
    message.value = 'ComfyUI 地址已保存'
  } catch (error) {
    message.value = error instanceof Error ? error.message : '保存失败'
  } finally {
    saving.value = false
  }
}

async function testConnection() {
  message.value = '正在测试连接...'
  try {
    const response = await fetch(`/api/settings/comfyui/test?url=${encodeURIComponent(comfyuiUrl.value)}`, { method: 'POST' })
    const data = await response.json()
    message.value = data.reachable ? `连接成功，延迟 ${data.latency_ms} ms` : `连接失败：${data.error}`
  } catch {
    message.value = '连接测试请求失败'
  }
}

onMounted(() => loadSettings().catch((error) => { message.value = error.message }))
</script>

<template>
  <main class="shell">
    <header>
      <p class="eyebrow">COMFYUI SERVER</p>
      <h1>作图服务控制台</h1>
      <p class="muted">第一阶段：服务配置与连接检查</p>
    </header>

    <section class="panel">
      <h2>ComfyUI 服务地址</h2>
      <p class="muted">内网 IP 变化后，可在这里更新，不需要重新构建容器。</p>
      <label for="comfyui-url">服务 URL</label>
      <input id="comfyui-url" v-model="comfyuiUrl" placeholder="http://192.168.1.20:8188" />
      <div class="actions">
        <button :disabled="saving" @click="saveSettings">{{ saving ? '保存中...' : '保存地址' }}</button>
        <button class="secondary" @click="testConnection">测试连接</button>
      </div>
      <p class="status" aria-live="polite">{{ message }}</p>
    </section>
  </main>
</template>
