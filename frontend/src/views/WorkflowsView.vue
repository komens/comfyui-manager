<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
const workflows = ref<Workflow[]>([])
onMounted(async () => { workflows.value = await api<Workflow[]>('/api/workflows') })
</script>
<template><PageHeader eyebrow="WORKFLOWS" title="工作流" description="工作流模板和参数映射将在这里维护。" /><section class="panel"><p v-if="!workflows.length" class="muted">暂无工作流，请先通过 API 导入 workflow JSON。</p><div v-for="workflow in workflows" :key="workflow.id" class="list-row"><strong>{{ workflow.name }}</strong><span>{{ workflow.description || '未填写描述' }}</span></div></section></template>
