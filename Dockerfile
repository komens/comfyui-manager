# Stage 1: Build frontend
FROM node:18-alpine AS frontend-build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci --ignore-scripts 2>/dev/null || npm install
COPY frontend/ ./
RUN npm run build

# Stage 2: Build backend
FROM golang:1.23 AS backend-build
WORKDIR /src
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=1.1.0" -o /out/comfyui-server ./cmd/server

# Stage 3: Final image
FROM debian:bookworm-slim
LABEL org.opencontainers.image.title="comfyui-server" \
      org.opencontainers.image.version="1.1.0" \
      org.opencontainers.image.description="ComfyUI workflow management and prompt library"
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/data /app/static

COPY --from=backend-build /out/comfyui-server /app/comfyui-server
COPY --from=frontend-build /app/dist /app/static

WORKDIR /app

ENV DATA_DIR=/app/data
ENV STATIC_DIR=/app/static
ENV SERVER_PORT=8080

EXPOSE 8080
ENTRYPOINT ["/app/comfyui-server"]
