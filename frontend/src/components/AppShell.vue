<script setup lang="ts">
import { RouterLink, RouterView } from 'vue-router'
</script>

<template>
  <div class="app-layout"><aside class="sidebar"><div class="brand"><span class="brand-mark">C</span><div><strong>ComfyUI</strong><small>作图服务</small></div></div><nav><RouterLink to="/">概览</RouterLink><RouterLink to="/submit">直接提交</RouterLink><RouterLink to="/json-files">JSON 文件</RouterLink><RouterLink to="/workflows">工作流</RouterLink><RouterLink to="/gallery">图片库</RouterLink><RouterLink to="/settings">设置</RouterLink></nav><p class="sidebar-note">个人 NAS 控制台</p></aside><main class="content"><RouterView /></main></div>
</template>
