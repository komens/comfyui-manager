<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'
const tasks = ref<Task[]>([])
onMounted(async () => { try { tasks.value = await api<Task[]>('/api/tasks') } catch { tasks.value = [] } })
</script>
<template><PageHeader eyebrow="OVERVIEW" title="作图概览" description="查看最近任务和服务状态。" /><section class="stats"><div class="stat"><span>最近任务</span><strong>{{ tasks.length }}</strong></div><div class="stat"><span>运行中</span><strong>{{ tasks.filter(t => t.status === 'running').length }}</strong></div><div class="stat"><span>已完成</span><strong>{{ tasks.filter(t => t.status === 'completed').length }}</strong></div></section><section class="panel"><div class="section-title"><h2>最近任务</h2><RouterLink to="/submit">新建任务</RouterLink></div><p v-if="!tasks.length" class="muted">暂无任务记录。</p><div v-for="task in tasks" :key="task.id" class="list-row"><span>#{{ task.id }} · {{ task.source_type }}</span><span class="status">{{ task.status }}</span></div></section></template>
