# vidlink 镜像
#
# 目标平台默认与树莓派一致（linux/arm64）。x86 上构建只需改 --platform。
#
#   docker build -t vidlink .
#   docker buildx build --platform linux/arm64,linux/amd64 -t vidlink --push .
#
# 镜像里只有静态二进制与 CA 证书：无 shell、无包管理器、非 root。

# ---------- 构建阶段 ----------
FROM golang:1.24-alpine AS build

WORKDIR /src

# 先只拷 go.mod：本模块零第三方依赖，但保留这一层是为了将来加依赖时
# 不会让每次改代码都重下依赖。
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 得到静态二进制，才能放进 scratch；
# -trimpath 去掉构建机的绝对路径，让构建可复现；
# -s -w 去掉符号表与调试信息（镜像里省几 MB）。
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.buildVersion=${VERSION}" \
      -o /out/vidlink .

# 运行期的账本目录（见下方 COPY --chown）
RUN mkdir -p /out/data

# ---------- 运行阶段 ----------
FROM scratch

# CA 根证书：访问 https 上游必需。没有它所有请求都会以 x509 错误失败。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/vidlink /vidlink

# 账本目录必须存在且属于运行用户，否则首次启动写账本会失败。
# scratch 里没有 mkdir，所以用 COPY --chown 把目录带进来。
COPY --from=build --chown=65534:65534 /out/data /app/data
WORKDIR /app
ENV VIDLINK_ACCOUNTS_PATH=/app/data/accounts.jsonl

# 以非 root 运行。65534 是 scratch 里惯用的 nobody。
USER 65534:65534

EXPOSE 8080

# 探针用 /healthz（存活）与 /readyz（就绪），二者都免鉴权。
HEALTHCHECK --interval=30s --timeout=3s --start-period=2s --retries=3 \
  CMD ["/vidlink", "-healthcheck=auto"]

ENTRYPOINT ["/vidlink"]
