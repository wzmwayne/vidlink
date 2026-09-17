# 离线 Docker 部署（不需要 Docker Hub）

给"目标机连不上 Docker Hub、也不装 Go"的场景用。目标机与构建机**同为
linux/arm64** 时最省事；换架构时把 `GOARCH` 改成目标架构即可。

为什么不用仓库根目录那个 `Dockerfile`：它用 `golang:1.24-alpine` 作构建阶段，
离线环境拉不到那个镜像。这里的做法是**在本地把二进制编好**，镜像只负责打包
（`FROM scratch`，不拉任何东西）。

## 一次性准备（构建机）

```bash
VERSION=$(git describe --tags --always)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w -X main.buildVersion=$VERSION" -o /tmp/vl/vidlink .
mkdir -p /tmp/vl/data
cp deploy/offline-docker/{Dockerfile,compose.yaml} /tmp/vl/
```

再补三样东西（都不进 Git）：

```bash
# ① 目标机的 CA 根证书（scratch 镜像里没有，访问 https 上游必需）
ssh user@host 'cat /etc/ssl/certs/ca-certificates.crt' > /tmp/vl/ca-certificates.crt
# ② 敏感配置：管理 Key 与抖音 Cookie，权限 600
cat > /tmp/vl/.env <<'ENV'
VIDLINK_ADMIN_KEY=vl_admin_请自己生成
VIDLINK_COOKIE_DOUYIN=UIFID_TEMP=...; ttwid=...
ENV
chmod 600 /tmp/vl/.env
```

## 部署（目标机）

```bash
scp -r /tmp/vl user@host:~/vidlink
ssh user@host 'cd ~/vidlink && mkdir -p vl-data data && docker compose up -d --build'
ssh user@host 'curl -s http://127.0.0.1:18080/v1/health'
```

## 说明

- **端口**：设备上 `127.0.0.1:18080`，容器内 `8080`。只绑回环是刻意的——
  对外只经 cloudflared 隧道。想让局域网直连就改成 `"18080:8080"`。
- **用户**：`user: "1000:1000"` 与宿主用户对齐，因为 `./vl-data` 属于他，
  而 scratch 里没有 `chown`、`sudo` 往往又要密码。想改成镜像内置的
  `65534:65534` 就得先 `sudo chown -R 65534:65534 vl-data`。
- **数据**：账本在 `./vl-data/accounts.jsonl`（append-only JSONL），
  换镜像/重启都不丢；`docker compose down` 也不会删它（没有用命名卷）。
- **cloudflared**：`compose.yaml` 里留了一段注释模板。拿到 tunnel token 后
  取消注释并填 token；Cloudflare 后台的 Public Hostname 里
  **Service 填 `http://vidlink:8080`**（同一 compose 网络里的容器名，
  不要填 `127.0.0.1:18080`——那在容器里指向它自己）。

  token 是凭据，两种放法都行：内联进 `compose.yaml` 后 `chmod 600 compose.yaml`
  （与 vaultwarden 那份写法一致），或写进 `.env` 再用 `${CLOUDFLARED_TOKEN}` 插值。
  起起来之后看日志确认隧道真的注册上了：

  ```bash
  docker logs -f <容器名> | grep -E "Registered tunnel connection|Updated to new configuration"
  ```

  **验证容器间连通性不用装 curl**：镜像自带探针，让一次性容器去拨目标地址即可
  （`-healthcheck=auto` 会用 `VIDLINK_ADDR` 推导出 URL）：

  ```bash
  docker run --rm --network <项目名>_default \
    -e VIDLINK_ADDR=vidlink:8080 vidlink:latest -healthcheck=auto; echo $?
  ```

- **升级**：本地重新编译 → `scp` 覆盖 `vidlink` → 目标机
  `docker compose up -d --build`。
