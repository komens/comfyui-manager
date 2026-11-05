# AI 作图接口

任务：生成提示词 → 提交作图 → 轮询 → 看图验证。

`{BASE}` 例 `http://192.168.1.10:18080`。无鉴权。JSON，写操作带 `Content-Type: application/json`。错误体 `{"error":"原因"}`。提交=入队，无回调。

## 1. 单个作图

```
1 GET  /api/health
2 GET  /api/workflows?page_size=50        取 enabled=true 的一条，记 id
3 POST /api/tasks/direct                  → 任务 id（字段名是 id）
4 GET  /api/tasks/{id}                    轮询至终态
5 GET  /api/images/{image_id}/preview     视觉校验
```

## 2. 批量作图

```
1 GET  /api/health
2 GET  /api/workflows?page_size=50
3 POST /api/json-files/import-text        整批提示词一次导入 → 用响应的 group_name
4 POST /api/prompts/group-run             按 group_name 整组提交
5 GET  /api/prompts?group={group_name}&page_size=100    轮询每条
6 GET  /api/images/{cover_image_id}/preview
```

禁止用 `tasks/direct` 循环做批量：会全部落进「手动提交」组，无法按批查询与复跑。

## 3. 接口

### GET /api/health
`{"status":"ok"}`

### GET /api/workflows?page_size=50

```json
{"items":[{"id":7,"name":"Pony-文生图-832x1216","description":"832×1216，dpmpp_2m/karras，cfg 6.5，30 步","negative_prompt":"","enabled":true,"is_default":false,"task_count":12,"in_flight":0}],"total":3,"page":1,"page_size":50}
```

按 `description`（尺寸/采样器/模型）选。`page_size` 取 1–200，越界静默回退 20。停用工作流提交会 400。

### POST /api/tasks/direct

```json
// 请求
{"workflow_id":7,"positive_prompt":"a lone lighthouse at sunset, cinematic lighting","title":"海边日落"}
// 202
{"id":101,"prompt_id":55,"status":"pending","comfyui_url":"http://192.168.1.20:8188"}
```

`workflow_id`、`positive_prompt` 必填；`title` 可省（默认取提示词前 50 字）。
**任务 id 在 `id` 字段，不是 `task_id`。**

### GET /api/tasks/{id}

```json
{"id":101,"status":"completed","total_count":1,"success_count":1,"failed_count":0,"created_at":"2026-11-05T14:30:00+08:00",
 "items":[{"id":88,"prompt_id":55,"positive_prompt":"a lone lighthouse...","status":"success","error_message":"","notes":"",
           "images":[{"id":301,"filename":"ComfyUI_00001_.png","width":832,"height":1216}]}]}
```

任务状态：`pending` → `running` → `completed` / `failed` / `cancelled`
条目状态：`pending` → `running` → `submitted` → `success` / `failed`
成功判定：`status=="completed" && success_count>=1 && items[].images` 非空。
`items[].images` 可能为空数组（失败或尚未取回）。失败原因在 `items[].error_message`。

### POST /api/json-files/import-text

```json
// 请求
{"filename":"AI-批量-20261105","content":"[{\"title\":\"雾中松林\",\"positive\":\"misty pine forest\"}]"}
// 201
{"filename":"AI-批量-20261105_20261105_143000.json","total":2,"inserted":2,"skipped_duplicates":0,"group_name":"AI-批量-20261105"}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `content` | 是 | **字符串**，内容为 JSON，≤10 MiB。传数组本体 → 400 `invalid request body` |
| `filename` | 否 | 决定 `group_name`：去扩展名、非法路径字符换成 `_`；缺省 `手动导入-年月日时分秒` |

`content` 两种等价形态：`{"entries":[{...}]}` 或 `[{...}]`。条目：`positive`（必填）、`title`（缺省取 `positive` 前 50 字）、`desc`、`id`（仅自用）。

- 任一 `positive` 空白 → 整批 400 `each entry requires a positive prompt`，不做部分导入
- 空数组 → 400 `no entries found in JSON`
- 去重键 `(group_name, positive)`，命中计入 `skipped_duplicates`；换 `filename` 即换组
- 顶层无 `negative` 字段，写了被忽略
- **不返回提示词 id**，验证走 `GET /api/prompts?group=`

### POST /api/prompts/group-run

```json
// 请求：group_name 用 import-text 返回值
{"group_name":"AI-批量-20261105","workflow_id":7}
// 200
{"created":8}
```

只提交组内 `pending` / `failed` 的条目，`created` 可小于组内总数（已完成的跳过，正常）。
要重跑已跑过的批次用 `batch-run`。

### POST /api/prompts/batch-run

```json
{"ids":[55,56,57],"workflow_id":7}
// 200
{"created":3}
```

不限状态。只回数量，不回任务 id。

### GET /api/prompts?group={group_name}&page_size=100

```json
{"items":[{"id":55,"title":"雾中松林","positive_prompt":"misty pine forest","group_name":"AI-批量-20261105","status":"done","run_count":1,"image_count":1,"cover_image_id":301,"completed_at":"2026-11-05T14:31:20+08:00"}],"total":8,"page":1,"page_size":100}
```

提示词状态：`pending` / `running` / `done` / `failed`
单条成功：`status=="done" && image_count>0`，取图用 `cover_image_id`。
单页最多 200 条：`total` 大于返回条数时递增 `page` 取全，否则会误判批次未跑完。
列表不含错误原因，失败要看 `GET /api/prompts/{id}` 的 `runs[]`：

```json
{"id":55,"status":"failed","runs":[{"task_id":101,"status":"failed","error_message":"submit to ComfyUI failed: ...","images":[]}]}
```

### 取图

`GET /api/images/{id}/preview` 长边 1600 WebP（校验用）；`/api/images/{id}/file` 原图（交付用，单张数 MB）。直接返回图片字节。

失败重试 `POST /api/tasks/{id}/retry`（仅 `failed`）；取消 `POST /api/tasks/{id}/cancel`（仅 `pending` / `running`）。

## 4. 轮询

- 间隔 3–5 秒（服务端 3 秒扫一次队列），单张通常 20–90 秒，超时按 10 分钟
- 只查未终态条目；批量先看整组，全部 `done`/`failed` 后再逐条查原因

## 5. 工作流管理

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/workflows` | 列表 |
| GET | `/api/workflows/{id}` | 详情，含完整 `workflow_json` |
| POST | `/api/workflows` | 最小体 `{"name":"...","workflow_json":{...}}` |
| PUT | `/api/workflows/{id}` | 字段均可选 |
| DELETE | `/api/workflows/{id}` | 有 pending/running 任务时 409 |
| PUT | `/api/workflows/{id}/default` | `{"is_default":true}`，全局唯一 |
| GET | `/api/workflows/{id}/prompt-targets` | 正/负向落点 |
| POST | `/api/workflows/{id}/validate` | 目标 ComfyUI 静态校验 |

- 改 `workflow_json` 必须同时传 `"mapping": {}`，否则沿用旧节点 id → 提交报「未能确定正向提示词落点」
- 无启用/停用接口：新建即启用，之后只能删

## 6. 限制

| 事项 | 说明 |
|---|---|
| `parameters` | 所有接口里传了都不生效，不要依赖 |
| 种子 | 每次随机，同提示词出图不同属正常 |
| 负向提示词 | 只取工作流的 `negative_prompt`，调用方不能逐次指定 |
| 并发 | 服务端串行消化队列，交 20 条 ≠ 同时跑 20 张 |
| 无鉴权 | 不要暴露公网 |
