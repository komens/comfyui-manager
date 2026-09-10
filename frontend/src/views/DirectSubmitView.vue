<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
const workflows = ref<Workflow[]>([]); const workflowId = ref(0); const positive = ref(''); const negative = ref(''); const message = ref(''); const busy = ref(false)
onMounted(async () => { workflows.value = await api<Workflow[]>('/api/workflows'); if (workflows.value[0]) workflowId.value = workflows.value[0].id })
async function submit() { busy.value = true; try { const data = await api<{ id: number }>('/api/tasks/direct', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({ workflow_id: workflowId.value, positive_prompt: positive.value, negative_prompt: negative.value, parameters: {} }) }); message.value = `任务 #${data.id} 已进入队列`; positive.value = '' } catch (e) { message.value = e instanceof Error ? e.message : '提交失败' } finally { busy.value = false } }
</script>
<template><PageHeader eyebrow="NEW TASK" title="直接提交" description="输入提示词并选择工作流生成图片。" /><section class="panel form-panel"><label for="workflow">工作流</label><select id="workflow" v-model="workflowId"><option v-for="item in workflows" :key="item.id" :value="item.id">{{ item.name }}</option></select><label for="positive">正向提示词</label><textarea id="positive" v-model="positive" rows="8" placeholder="输入正向提示词" /><label for="negative">负向提示词</label><textarea id="negative" v-model="negative" rows="4" placeholder="输入负向提示词" /><button :disabled="busy || !workflowId || !positive.trim()" @click="submit">{{ busy ? '提交中...' : '提交作图' }}</button><p class="status">{{ message }}</p></section></template>
