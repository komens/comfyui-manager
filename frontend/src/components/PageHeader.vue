<script setup lang="ts">
defineProps<{ title: string; description?: string }>()
</script>

<template>
  <header class="page-header">
    <h1>{{ title }}</h1>
    <p v-if="description" class="description">{{ description }}</p>
  </header>
</template>
