# Stage 1: Build frontend
# 必须 Node 20.11+：unplugin 3.x 用 import.meta.dirname，Node 18 下是 undefined，
# 会在加载 vite.config.ts 时报 "The paths[0] argument must be of type string"。
FROM node:22-alpine AS frontend-build
WORKDIR /app
# npm 源：构建容器里没有宿主代理，直连 registry.npmjs.org 会被 TLS reset（卡在 npm ci 不动）。
# 默认走国内镜像；需要官方源时 --build-arg NPM_REGISTRY=https://registry.npmjs.org/
ARG NPM_REGISTRY=https://registry.npmmirror.com
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm config set registry "$NPM_REGISTRY" \
 && (npm ci --ignore-scripts || npm install)
COPY frontend/ ./
RUN npm run build

# Stage 2: Build backend
FROM golang:1.23 AS backend-build
WORKDIR /src
# 同上：容器内访问 proxy.golang.org 不通，默认用 goproxy.cn
ARG GOPROXY_URL=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY_URL}
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
# -tags nodynamic：禁用 dlopen 系统 libwebp 的分支，固定走内置纯 Go 版（wasm2go 转译）。
# 镜像里没有 libwebp，而该库的动态分支在编码失败时**是直接返回错误、不会回退**到纯 Go 版，
# 所以这个 tag 不能漏：漏了会让缩略图编码每次都失败（接口只能回退原图）。
# 二进制体积不变（wasm 版本来就在）。
RUN CGO_ENABLED=0 GOOS=linux go build -tags nodynamic -ldflags="-s -w -X main.version=1.11.2" -o /out/comfyui-server ./cmd/server

# Stage 3: Final image
# 基线用 alpine 而不是 debian:bookworm-slim。这个镜像里唯一需要包管理器的地方就是
# 解释器：debian 那套（slim 75MB + python3 46MB）合计约 121MB，alpine 对应约 51MB。
# 后端是 CGO_ENABLED=0 静态编译，不依赖 glibc，换 musl 没有兼容问题。
#
# python3 只服务脚本运行台（ENABLE_SCRIPTS）。不开这个功能时它白占约 39MB，
# 但「换镜像版本才能装」会把临时想跑个脚本变成一次 rebuild，不值。
# 确实不需要这一项时用 --build-arg WITH_PYTHON=0（镜像再省 39MB，脚本运行台随之不可用）。
#
# bash 保留：脚本运行台对 .sh 优先用 bash 执行，alpine 默认只有 busybox sh，
# 缺 bash 会让 [[ ]] 之类的写法静默失败。
#
# alpine 的 python 是 musl 版：pip 拉来的 manylinux 预编译 wheel（numpy/pandas 等）
# 用不了。要第三方库请用 apk 装 —— py3-numpy、py3-pillow、py3-pip 都是现成的。
#
# tzdata 保留：程序里时间走 time.Now()，缺 zoneinfo 时设了 TZ 也不生效。
FROM alpine:3.22

ARG WITH_PYTHON=1
# apk 源：dl-cdn.alpinelinux.org 在国内经常几 MB 的包拉几分钟还断（python3-pycache 报 IO ERROR），
# 换成阿里云镜像。要回到官方源：--build-arg APK_MIRROR=dl-cdn.alpinelinux.org
ARG APK_MIRROR=mirrors.aliyun.com

LABEL org.opencontainers.image.title="comfyui-server" \
      org.opencontainers.image.version="1.11.2" \
      org.opencontainers.image.description="ComfyUI workflow management and prompt library"

RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache \
      ca-certificates \
      tzdata \
      bash \
    && if [ "$WITH_PYTHON" != "0" ]; then apk add --no-cache python3; fi \
    && mkdir -p /app/data /app/static

COPY --from=backend-build /out/comfyui-server /app/comfyui-server
COPY --from=frontend-build /app/dist /app/static

WORKDIR /app

ENV DATA_DIR=/app/data
ENV STATIC_DIR=/app/static
ENV SERVER_PORT=8080

EXPOSE 8080
ENTRYPOINT ["/app/comfyui-server"]
