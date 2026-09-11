import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import DirectSubmitView from '../views/DirectSubmitView.vue'
import JsonFilesView from '../views/JsonFilesView.vue'
import WorkflowsView from '../views/WorkflowsView.vue'
import SettingsView from '../views/SettingsView.vue'
import TaskDetailView from '../views/TaskDetailView.vue'
import GalleryView from '../views/GalleryView.vue'
import PromptsView from '../views/PromptsView.vue'
import PromptDetailView from '../views/PromptDetailView.vue'
import PromptEditView from '../views/PromptEditView.vue'
import TaskListView from '../views/TaskListView.vue'

export default createRouter({ history: createWebHistory(), routes: [
  { path: '/', component: DashboardView },
  { path: '/submit', component: DirectSubmitView },
  { path: '/prompts', component: PromptsView },
  { path: '/prompts/new', component: PromptEditView },
  { path: '/prompts/:id', component: PromptDetailView },
  { path: '/prompts/:id/edit', component: PromptEditView },
  { path: '/json-files', component: JsonFilesView },
  { path: '/workflows', component: WorkflowsView },
  { path: '/tasks', component: TaskListView },
  { path: '/tasks/:id', component: TaskDetailView },
  { path: '/gallery', component: GalleryView },
  { path: '/settings', component: SettingsView },
  { path: '/:pathMatch(.*)*', redirect: '/' },
] })
