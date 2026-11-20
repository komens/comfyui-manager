<script setup lang="ts" generic="T">
import ViewFeedback from './ViewFeedback.vue'
defineProps<{
  items: T[]
  recordKey: (item: T) => string | number
  emptyMessage: string
}>()
</script>

<template>
  <template v-if="items.length">
    <div class="records-table"><slot /></div>
    <ul class="compact-records">
      <li v-for="item in items" :key="recordKey(item)" class="compact-record">
        <div class="compact-summary">
          <div class="compact-title"><slot name="title" :item="item" /></div>
          <div class="compact-actions"><slot name="actions" :item="item" /></div>
        </div>
        <el-collapse class="compact-details">
          <el-collapse-item name="details" title="查看详情">
            <slot name="details" :item="item" />
          </el-collapse-item>
        </el-collapse>
      </li>
    </ul>
  </template>
  <ViewFeedback v-else kind="empty" :message="emptyMessage" />
</template>

<style scoped>
.compact-records { display: none; }
@media (max-width: 640px) {
  .records-table { display: none; }
  .compact-records { display: block; margin: 0; padding: 0; list-style: none; }
  .compact-record { padding: 12px 14px; border-bottom: 1px solid var(--c-border-light); }
  .compact-record:last-child { border-bottom: 0; }
  .compact-summary { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: start; gap: 8px; }
  .compact-title { overflow-wrap: anywhere; }
  .compact-actions { display: flex; flex-wrap: wrap; justify-content: end; }
  .compact-actions :deep(.el-button) { margin-left: 0; padding-inline: 6px; }
  .compact-details { margin-top: 4px; border: 0; }
  .compact-details :deep(.el-collapse-item__header) { height: 32px; border: 0; color: var(--c-primary); }
  .compact-details :deep(.el-collapse-item__content) { padding-bottom: 4px; overflow-wrap: anywhere; }
}
</style>
