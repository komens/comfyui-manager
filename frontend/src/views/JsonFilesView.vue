<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { RouterLink } from 'vue-router'
type JsonFile = { id: number; filename: string; total_count: number; completed_count: number; failed_count: number }
const files = ref<JsonFile[]>([]); const message = ref('')
async function load() { files.value = await fetch('/api/json-files').then(r => r.json()) }
async function upload(event: Event) { const input = event.target as HTMLInputElement; if (!input.files?.[0]) return; const body = new FormData(); body.append('file', input.files[0]); const response = await fetch('/api/json-files/upload', { method: 'POST', body }); const data = await response.json(); message.value = response.ok ? `已导入 ${data.filename}` : data.error; await load() }
onMounted(load)
</script>
<template><PageHeader eyebrow="PROMPT DATA" title="JSON 文件" description="上传提示词数据并查看生成状态。" /><section class="toolbar"><label class="upload-button">导入 JSON<input type="file" accept=".json,application/json" @change="upload" /></label><span class="muted">{{ message }}</span></section><section class="panel"><div v-if="!files.length" class="muted">暂无 JSON 文件。</div><RouterLink v-for="file in files" :key="file.id" :to="`/json-files/${file.id}`" class="list-row task-link"><strong>{{ file.filename }}</strong><span>{{ file.completed_count }} / {{ file.total_count }} 已完成 · {{ file.failed_count }} 失败</span></RouterLink></section></template>
