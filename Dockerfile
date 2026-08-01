# syntax=docker/dockerfile:1
# PacketLab — 多阶段构建
# 阶段1: 构建含 pcap (CGO) 的二进制，运行时仅需 libpcap
FROM golang:1.25-bookworm AS builder

# 网卡抓包需要 libpcap 开发头文件
RUN apt-get update && apt-get install -y --no-install-recommends \
    libpcap-dev gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 启用 CGO 以获得完整 pcap 网卡抓包支持
ARG TARGETARCH
ENV CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH:-amd64}
RUN go build -trimpath -ldflags="-s -w" -o /out/packetlab ./cmd/proxy/

# ---- 运行时镜像 ----
FROM debian:bookworm-slim

# 运行时仅需 libpcap 运行库（非 dev 包）
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates libpcap0.8 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /out/packetlab /app/packetlab

# 数据 & 证书持久化目录（非 root 用户 packetlab）
RUN useradd --uid 1000 --create-home packetlab \
    && mkdir -p /home/packetlab/.packetlab/certs \
    && chown -R packetlab:packetlab /home/packetlab/.packetlab
VOLUME ["/home/packetlab/.packetlab"]

# 代理端口 8080，Web/API 端口 9090
EXPOSE 8080 9090

USER packetlab
ENTRYPOINT ["/app/packetlab"]
# 默认参数：启动代理 + Web，网卡抓包需显式 --capture（且容器需 NET_ADMIN/CAP_NET_RAW）
CMD ["--proxy-port", "8080", "--api-port", "9090"]
