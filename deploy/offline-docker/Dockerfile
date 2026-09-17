# vidlink 运行镜像（离线打包版）
#
# 为什么不复用仓库里的 Dockerfile：那个用 golang:1.24-alpine 作构建阶段，
# 而本机 Docker Hub 直连不通、也没缓存 golang 镜像。目标机与本机同为
# arm64，二进制在本地编译好带过来，这里只负责打包。
FROM scratch

# CA 根证书：访问 https 上游必需（从宿主机 /etc/ssl/certs 拷进来的那份）
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

COPY vidlink /vidlink

# 账本目录；compose 里用 ./vl-data 覆盖它
COPY --chown=1000:1000 data /app/data

WORKDIR /app
ENV VIDLINK_ACCOUNTS_PATH=/app/data/accounts.jsonl

# 以宿主用户（wayne = uid 1000）运行：bind mount 的目录属于他，
# 而 scratch 里没有 chown、sudo 又要密码，这样最省事且仍是"非 root"。
USER 1000:1000

EXPOSE 8080

# 探针：scratch 里没有 curl，用二进制自带的 -healthcheck=auto
HEALTHCHECK --interval=30s --timeout=3s --start-period=2s --retries=3 \
  CMD ["/vidlink", "-healthcheck=auto"]

ENTRYPOINT ["/vidlink"]
