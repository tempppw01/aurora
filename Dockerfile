# syntax=docker/dockerfile:1.7
#
# 多阶段构建。使用标准 Docker 指令，兼容 Railway Metal builder；
# 依赖层仍可由 Docker 的层缓存复用。
#
# 注意:必须先 COPY go.mod/go.sum 再 go mod download,
# 这样 buildx 才能在 go.sum 变化时失效此层缓存,触发重新下载。
ARG GO_VERSION=1.26.0

# ---- 阶段 1: 编译 ----
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# 1) 先拷贝 module 清单(几乎不变,缓存命中率最高)
COPY go.mod go.sum ./
# 2) 下载依赖
RUN go mod download

# 3) 拷贝其余源码(改动频繁,缓存粒度细)
COPY . .

# buildx 多架构构建时自动注入 TARGETOS / TARGETARCH
ARG TARGETOS TARGETARCH
# 静态二进制,避免 CGO 跨架构链接;同时让 go build 启用
# 完全确定性的输出(trimpath 剥路径前缀)
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# 4) 编译
RUN go build -trimpath -ldflags='-s -w -buildid=' -o /out/aurora .

# ---- 阶段 2: 运行镜像(distroless,~2MB,无 shell 更安全)----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aurora /aurora
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/aurora"]
