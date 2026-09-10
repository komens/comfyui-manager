# ComfyUI 可视化作图服务

## 第一阶段启动

启动后端：

```bash
cd backend
go run ./cmd/server
```

启动前端开发服务：

```bash
cd frontend
npm install
npm run dev
```

打开 `http://localhost:5173`，可以查看并修改 ComfyUI 地址、测试连接。

默认配置：

```text
COMFYUI_URL=http://127.0.0.1:8188
SERVER_PORT=8080
DATA_DIR=../data
```

ComfyUI 地址首次从 `COMFYUI_URL` 读取并保存到 SQLite。之后通过页面修改即可，不需要重启服务。

## Docker

```bash
docker compose up -d --build
```

数据保存在项目根目录的 `data/`，包括数据库、JSON、workflow、图片和日志目录。
