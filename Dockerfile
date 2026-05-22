# ─────────────────────────────────────────────
# Stage 1: Builder — 静态编译阶段
# ─────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

# git 用于 go mod download 拉取依赖，ca-certificates 用于 HTTPS
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# 优先复制依赖声明，利用 Docker 层缓存加速重复构建
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# 复制全部源码
COPY *.go ./

# 静态编译：禁用 CGO，-ldflags="-w -s" 去除调试符号压缩体积，-trimpath 移除本地路径信息
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -trimpath -o /build/matrix-asset-engine .

# ─────────────────────────────────────────────
# Stage 2: Runtime — 最终运行镜像（目标体积 < 30MB）
# ─────────────────────────────────────────────
FROM alpine:latest

# CA 证书（HTTPS 请求必须）与时区数据
RUN apk add --no-cache ca-certificates tzdata

# 时区对齐 CronJob 时间轴
ENV TZ=Asia/Shanghai

# 最小权限原则：创建非 root 运行用户
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# 从 builder 阶段复制编译产物
COPY --from=builder /build/matrix-asset-engine .

# 切换至非 root 用户
USER appuser

# 运行时通过 -e 或 Kubernetes Secret 注入
ENV WEBHOOK_URL=""

ENTRYPOINT ["/app/matrix-asset-engine"]
