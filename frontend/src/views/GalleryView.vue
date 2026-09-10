<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
type ImageItem = { id: number; filename: string; path: string; created_at: string }
const images = ref<ImageItem[]>([])
onMounted(async () => { images.value = await fetch('/api/images').then(r => r.json()) })
</script>
<template><PageHeader eyebrow="IMAGE LIBRARY" title="图片库" description="查看已下载的生成结果。" /><section v-if="!images.length" class="panel muted">暂无生成图片。</section><section v-else class="image-grid"><article v-for="image in images" :key="image.id" class="image-item"><div class="image-placeholder">{{ image.filename }}</div><small>{{ image.created_at }}</small></article></section></template>
