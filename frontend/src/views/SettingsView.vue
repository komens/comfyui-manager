<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api } from '../api/client'
const url = ref(''); const message = ref(''); const busy = ref(false)
onMounted(async () => { const data = await api<{ comfyui_url: string }>('/api/settings'); url.value = data.comfyui_url })
async function save() { busy.value = true; try { const data = await api<{ comfyui_url: string }>('/api/settings/comfyui', { method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({ url: url.value }) }); url.value = data.comfyui_url; message.value = '地址已保存' } catch (e) { message.value = e instanceof Error ? e.message : '保存失败' } finally { busy.value = false } }
async function test() { message.value = '测试中...'; try { const data = await api<{ latency_ms: number }>('/api/settings/comfyui/test?url=' + encodeURIComponent(url.value), { method: 'POST' }); message.value = `连接成功，延迟 ${data.latency_ms} ms` } catch (e) { message.value = e instanceof Error ? e.message : '连接失败' } }
</script>
<template><PageHeader eyebrow="SETTINGS" title="服务设置" description="内网 IP 变化后，在此更新 ComfyUI 地址。" /><section class="panel form-panel"><label for="url">ComfyUI 服务地址</label><input id="url" v-model="url" placeholder="http://192.168.1.20:8188" /><div class="actions"><button :disabled="busy" @click="save">{{ busy ? '保存中...' : '保存地址' }}</button><button class="secondary" @click="test">测试连接</button></div><p class="status">{{ message }}</p></section></template>
