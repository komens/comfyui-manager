# 后端-only 镜像：distroless、**不含前端静态文件**，适合单独跑 API / 二次集成。
# 完整可部署镜像请用仓库根目录的 Dockerfile（alpine + 内置前端 + 注入版本号），
# docker-compose.yml 用的也是那一份。
#
# 注意：这里没有 -X main.version=...，所以 GET /api/version 会返回 "dev"。
#
# 注意：distroless/static 里没有 shell 也没有 python3，所以 **ENABLE_SCRIPTS 在这份
# 镜像上没有意义**（打开后所有运行都会报「容器内没有解释器」）。要脚本运行台请用仓库
# 根目录那份 Dockerfile。
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/comfyui-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/comfyui-server /comfyui-server
EXPOSE 8080
ENTRYPOINT ["/comfyui-server"]
