<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  kind?: 'loading' | 'empty' | 'success' | 'error' | 'info' | 'warning'
  message?: string
}>(), { kind: 'info' })

const isState = computed(() => props.kind === 'loading' || props.kind === 'empty')
const alertType = computed(() => props.kind === 'loading' || props.kind === 'empty' ? 'info' : props.kind)
</script>

<template>
  <div v-if="isState || message" class="view-feedback" :class="{ 'view-feedback-state': isState }">
    <el-skeleton v-if="kind === 'loading'" animated :rows="3" role="status" :aria-label="message || '正在加载'" />
    <el-empty v-else-if="kind === 'empty'" :description="message || '暂无数据'" :image-size="72">
      <slot />
    </el-empty>
    <el-alert v-else :type="alertType" :closable="false" show-icon :role="kind === 'error' ? 'alert' : 'status'">
      <template #title>{{ message }}</template>
      <slot />
    </el-alert>
  </div>
</template>

<style scoped>
.view-feedback { margin-block: 10px; overflow-wrap: anywhere; }
.view-feedback-state { margin: 0; padding: 16px; }
.view-feedback :deep(.el-alert__title) { white-space: pre-line; }
</style>
