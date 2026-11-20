<script setup lang="ts">
import { ref, useId } from 'vue'
import { Filter } from '@element-plus/icons-vue'

defineProps<{ active: string[] }>()
const emit = defineEmits<{ clear: [] }>()
const expanded = ref(false)
const filtersId = useId()
</script>

<template>
  <div class="filter-toolbar">
    <div class="toolbar filter-controls">
      <slot />
      <el-button
        class="mobile-filter-toggle"
        :icon="Filter"
        :aria-controls="filtersId"
        :aria-expanded="expanded"
        @click="expanded = !expanded"
      >{{ expanded ? '收起筛选' : '筛选' }}</el-button>
      <div :id="filtersId" class="secondary-filters" :class="{ expanded }">
        <slot name="secondary" />
      </div>
    </div>
    <div v-if="active.length" class="active-filters" aria-label="当前筛选">
      <el-tag v-for="label in active" :key="label" effect="plain" size="small" :title="label">{{ label }}</el-tag>
      <el-button text size="small" @click="emit('clear')">清空筛选</el-button>
    </div>
  </div>
</template>

<style scoped>
.filter-toolbar { margin-bottom: 12px; }
.filter-controls { margin-bottom: 0; gap: 8px; }
.mobile-filter-toggle { display: none; }
.secondary-filters, .active-filters { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; min-width: 0; }
.active-filters { margin-top: 8px; }
.active-filters .el-tag { max-width: 100%; }
.active-filters :deep(.el-tag__content) { overflow: hidden; text-overflow: ellipsis; }
@media (max-width: 768px) {
  .filter-controls :deep(.toolbar-search) { flex-basis: 100%; }
  .mobile-filter-toggle { display: inline-flex; }
  .secondary-filters { display: none; flex-basis: 100%; }
  .secondary-filters.expanded { display: flex; }
  .secondary-filters :deep(.filter-select) { flex: 1 1 130px; min-width: 0; }
}
</style>
