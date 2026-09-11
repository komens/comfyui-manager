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
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/comfyui-server ./cmd/server

# Stage 3: Final image
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend-build /out/comfyui-server /comfyui-server
COPY --from=frontend-build /app/dist /static
ENV STATIC_DIR=/static
EXPOSE 8080
ENTRYPOINT ["/comfyui-server"]
