<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
const workflows = ref<Workflow[]>([]); const name = ref(''); const description = ref(''); const workflowJSON = ref('{}'); const mapping = ref('{}'); const message = ref('')
async function load() { workflows.value = await api<Workflow[]>('/api/workflows') }
async function create() { try { await api('/api/workflows', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({ name: name.value, description: description.value, workflow_json: JSON.parse(workflowJSON.value), mapping: JSON.parse(mapping.value) }) }); message.value = '工作流已保存'; name.value = ''; await load() } catch (e) { message.value = e instanceof Error ? e.message : '保存失败' } }
onMounted(load)
</script>
<template><PageHeader eyebrow="WORKFLOWS" title="工作流" description="保存 workflow JSON、节点映射和默认参数。" /><section class="panel form-panel"><h2>新增工作流</h2><label>名称</label><input v-model="name" placeholder="例如：普通人像" /><label>描述</label><input v-model="description" placeholder="可选" /><label>Workflow JSON</label><textarea v-model="workflowJSON" rows="7" spellcheck="false" /><label>节点映射 JSON</label><textarea v-model="mapping" rows="7" spellcheck="false" /><button :disabled="!name.trim()" @click="create">保存工作流</button><p class="status">{{ message }}</p></section><section class="panel workflow-list"><h2>已保存工作流</h2><p v-if="!workflows.length" class="muted">暂无工作流。</p><div v-for="workflow in workflows" :key="workflow.id" class="list-row"><strong>{{ workflow.name }}</strong><span>{{ workflow.description || '未填写描述' }}</span></div></section></template>
