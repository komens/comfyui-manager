<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
type Entry = { id: number; entry_key: string; title: string; description: string; status: string }
const route = useRoute(); const entries = ref<Entry[]>([]); const workflows = ref<Workflow[]>([]); const selected = ref<number[]>([]); const workflowId = ref(0); const mode = ref('pending'); const message = ref('')
async function load() { entries.value = await api<Entry[]>(`/api/json-files/${route.params.id}/entries`); workflows.value = await api<Workflow[]>('/api/workflows'); if (workflows.value[0]) workflowId.value = workflows.value[0].id }
async function submit() { try { const data = await api<{ count: number }>('/api/tasks/json', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({ json_file_id: Number(route.params.id), workflow_id: workflowId.value, entry_ids: selected.value, mode: mode.value, parameters: {} }) }); message.value = `已提交 ${data.count} 个任务` } catch (e) { message.value = e instanceof Error ? e.message : '提交失败' } }
onMounted(load)
</script>
<template><PageHeader eyebrow="PROMPT ENTRIES" :title="`JSON 文件 #${route.params.id}`" description="选择条目并批量提交生成。" /><section class="panel"><div class="actions"><select v-model="workflowId"><option v-for="item in workflows" :key="item.id" :value="item.id">{{ item.name }}</option></select><select v-model="mode"><option value="pending">仅未完成</option><option value="all">全部重新生成</option></select><button :disabled="!workflowId" @click="submit">提交选中条目</button></div><label class="check-all"><input v-model="selected" type="checkbox" :value="0" @change="selected = selected.includes(0) ? entries.map(e => e.id) : []" /> 全选</label><div v-for="entry in entries" :key="entry.id" class="list-row"><label class="entry-check"><input v-model="selected" type="checkbox" :value="entry.id" /> <span><strong>{{ entry.title || entry.entry_key }}</strong><small>{{ entry.description }}</small></span></label><span class="status">{{ entry.status }}</span></div><p class="status">{{ message }}</p></section></template>
