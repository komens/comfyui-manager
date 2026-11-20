import { ElButton, ElMessage } from 'element-plus'
import 'element-plus/es/components/message/style/css'
import { h } from 'vue'
import type { Router } from 'vue-router'

export function showTaskSubmissionMessage(router: Router, message: string, label: string, to: string) {
  const notice = ElMessage({
    type: 'success',
    duration: 3000,
    message: h('span', [
      message,
      h(ElButton, {
        size: 'small',
        type: 'success',
        link: true,
        class: 'task-submission-link',
        onClick: () => {
          notice.close()
          void router.push(to)
        },
      }, () => label),
    ]),
  })
}
