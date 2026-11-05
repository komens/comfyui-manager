<script setup lang="ts">
import { computed } from 'vue'
import { platformMeta } from '../utils/platforms'

const props = withDefaults(defineProps<{ platform: string; size?: 'sm' | 'md' }>(), { size: 'sm' })

const meta = computed(() => platformMeta(props.platform))
</script>

<template>
  <span
    v-if="platform"
    class="platform-badge"
    :class="`platform-badge-${size}`"
    :style="{ background: meta.color }"
    :title="`来源：${meta.label}`"
  >{{ meta.label }}</span>
</template>

<style scoped>
.platform-badge {
  display: inline-flex;
  align-items: center;
  color: #fff;
  font-weight: 600;
  border-radius: 999px;
  line-height: 1;
  letter-spacing: 0.02em;
  white-space: nowrap;
  flex-shrink: 0;
}
.platform-badge-sm { font-size: 10px; padding: 3px 7px; }
.platform-badge-md { font-size: 12px; padding: 4px 10px; }
</style>
