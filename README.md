# vidlink

极轻量、高并发、零第三方依赖的**多平台视频解析 API 服务**（Go）。

支持抖音、哔哩哔哩、快手、小红书。返回**平台已发布的干净直链**（抖音取无水印源）。

- 四个计量接口：`info` / `links` / `detail` / `batch_links`，按**配额**计量（配额由管理员分配，**与金额、支付无关**）；
- 内存 ≈ 12 MB、二进制 7 MB、零依赖，可直接跑在 1 GB 的树莓派上；
- 数据面零代理（默认只返回直链），服务端不搬运视频字节。

---

## 为什么是"极轻量"

| 指标 | 本实现 | 说明 |
|---|---|---|
| 第三方依赖 | **0 个** | 只用 Go 标准库；`go.mod` 没有任何 require |
| 二进制大小 | **7.0 MB** | `-ldflags "-s -w"` 后；无 CGO |
| 常驻内存 | **≈ 12 MB** | 50 并发实测 11.8~14.1 MB VmRSS、12 个线程；不缓存视频字节 |
| 启动时间 | 毫秒级 | 无框架初始化、无数据库连接 |

能做到零依赖的关键在于三件事都自己实现了：

- **HTTP 路由**：Go 1.22+ 的 `net/http.ServeMux` 已支持 `GET /v1/links` 这类模式，不需要 gin/echo；
- **JSON 取值**：自写 `jsonx`，支持多路径回退与 JS 兼容容错，不需要 gjson；
- **签名算法**：纯 Go 复刻 SM3、抖音 `a_bogus` / `x-secsdk-web-signature`、B 站 WBI，
  **不需要 JS 引擎或浏览器**。

## 为什么是"高并发 / 高效率"

1. **数据面零代理**（默认）：API 只返回 CDN 直链，服务端不搬运视频字节。
2. **请求合并（singleflight）+ TTL 缓存**：同一视频的 N 个并发请求只打上游 1 次。
3. **按主机令牌桶限流**：不同平台容忍度差异极大，按主机隔离调速，慢平台不拖垮快平台。
4. **四层并发控制**：全局解析槽位（10）→ 同请求合并 → 按主机令牌桶 → 连接池复用；
   另有按 Key 串行闸门（防止单个账号打满全局槽位）。
5. **惰性过期缓存**：无后台清理 goroutine，16 分片降低锁竞争。

实测（本机，B 站视频）：首次请求约 700 ms（真实走完 `view` + `playurl`），
第二次约 1 ms（命中缓存）。

## 快速开始

```bash
# 构建
go build -o vidlink .

# 运行（默认监听 :8080，账本写到 ./data/accounts.jsonl）
./vidlink
# → 日志里会打印一次性管理员 Key（或自己指定 VIDLINK_ADMIN_KEY）

export KEY=vl_admin_xxx

# ① 先看有哪些清晰度（0.5 配额）
curl -H "X-API-Key: $KEY" \
  'http://127.0.0.1:8080/v1/info?url=https://www.bilibili.com/video/BV1GJ411x7h7'

# ② 取一条能播的直链（1.0 配额）
curl -H "X-API-Key: $KEY" \
  'http://127.0.0.1:8080/v1/links?url=https://www.bilibili.com/video/BV1GJ411x7h7&quality=1080'

# ③ 元信息 + 全部档位直链（1.2 配额）
curl -H "X-API-Key: $KEY" \
  'http://127.0.0.1:8080/v1/detail?url=https://www.bilibili.com/video/BV1GJ411x7h7'

# ④ 批量取直链（0.75/条，5~20 条，不支持抖音）
curl -X POST http://127.0.0.1:8080/v1/batch/links \
  -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"text":"https://b23.tv/xxx\nhttps://b23.tv/yyy\nhttps://b23.tv/zzz\nhttps://b23.tv/aaa\nhttps://b23.tv/bbb"}'

# ⑤ 查看自己的配额与用量
curl -H "X-API-Key: $KEY" http://127.0.0.1:8080/v1/usage
```

## 免校验模式（VL_EASE=true）

想把它当**纯解析工具**用——不建账号、不发 Key、不算配额——加一个环境变量：

```bash
VL_EASE=true ./vidlink

# 不用任何凭据，直接解析
curl 'http://127.0.0.1:8080/v1/links?url=https://www.bilibili.com/video/BV1GJ411x7h7'
```

**浏览器打开 `http://127.0.0.1:8080/` 就是一个图形化解析页**：填链接点按钮即可，
结果按"直链 / 请求头 / 视频轨 / 音频轨 / 字幕 / 图集 / 原始 JSON"分区渲染，
带复制直链、复制 curl / ffmpeg 命令、批量解析（5~20 条）、平台能力表。

### 浏览器内混流（不需要 ffmpeg）

B 站这类平台返回的是 DASH 分离流（画面与声音两个文件）。
页面上的「下载并混流」按钮可以**在浏览器里把两条轨合成一个 MP4**：

```
流式下载视频轨 ─┐
                ├─→ 写入浏览器存储（OPFS，分片落盘）─→ 片段级重排 ─→ 保存到本地
流式下载音频轨 ─┘
```

- **防爆内存**：下载分片写进 OPFS，混流时每次只读一个 `moof`（约 1KB）与一片 `mdat`
  （按 Blob 切片直接转写，不进 JS 堆），输出同样写 OPFS。
  内存占用与文件大小无关，4K 长视频也不会把标签页撑爆；
- **不重新编码**：fMP4 的片段自带 `tfhd/tfdt/trun`，合并只是把两个 `moov`
  合成一个（两个 `trak` + 两个 `trex`）再按解码时间交叉排列，
  实测 1080P（67MB+5.2MB）在本机 **约 11 秒**跑完，速度只取决于下载带宽；
- **自己实现，不引第三方**：外链 mp4box.js 会破坏"离线可用"，
  ffmpeg.wasm 光 wasm 就 25MB 起步——为了"合并两段流"不值得把服务撑大几十倍。

实测（真实 B 站视频，2026-09）：产物 `ffprobe` 报
`h264 1280x720 + aac 48000Hz 立体声，时长 212.31s`，`ffmpeg -f null -` 全片解码零错误。

**两条实测得来的兜底逻辑**（都写在页面里）：

1. **CDN 节点差异**：多数节点带 `Access-Control-Allow-Origin: *`，浏览器可直连；
   但部分 B 站 P2P 节点（如 `*.edge.mountaintoys.cn`）**同时要求 Referer 与 UA**，
   缺任一个都 403，而这两个头浏览器禁止脚本设置。
   页面因此是三级兜底：直连 → 重新取链换节点 → 走 `/v1/proxy`
   （服务端补 Referer/UA；需要 `VIDLINK_PROXY_ENDPOINT=true` 与白名单）。
2. **预览可能不可用**：不含专有编解码器的 Chromium 构建
   （`canPlayType('avc1') === ''`）根本解不了 H.264。
   这种情况下页面会明确告诉你"文件有效，请保存后用本地播放器打开"，
   而不是让你以为合出来的文件坏了。

页面由服务端内嵌在二进制里（`internal/server/ui.html`，约 25 KB）：

- **零外部资源**——没有 CDN、字体、图标库，内网/离线打开也完整可用；
- 只用原生 `fetch` 调本服务的公开接口，**与 curl 能做的事完全一致**，页面不引入任何隐藏能力；
- 接口返回的标题/作者等文本一律用 `textContent` 写入，不构成 XSS 注入点。

打开后**账户体系整体关闭**，不是"跳过校验"而是"不存在"：

| 能力 | 账户模式（默认） | `VL_EASE=true` |
|---|---|---|
| API Key 校验 | 有（403） | **无**（不需要任何凭据） |
| 账号账本 | 读写 `data/accounts.jsonl` | **完全不建立**（不读也不写文件） |
| 初始管理员 | 自动创建并打印 Key | **不创建** |
| 配额计量与 `X-Quota-*` | 有 | **无**（不回写这两个头） |
| 按 Key 串行闸门 | 1 个并发解析 | 不适用（没有 Key） |
| 按 IP 限流 | `VIDLINK_RATE_LIMIT_RPM` | **关闭**（连同它一起关） |
| `/v1/usage`、`/v1/admin/*` | 存在 | **404**（根本不注册） |
| `/v1/info`、`/v1/links`、`/v1/detail`、`/v1/batch/links` | 需 Key | **完全可用** |
| `/v1/platforms`、`/v1/version`、`/v1/health`、探针 | 公开 | 公开（不再返回 `rates`/`unit`） |
| 全局解析槽位（10 + 队列 30） | 有 | **保留**（这是资源保护，不是校验） |

两点刻意的设计：

- **保留全局解析槽位与按主机令牌桶。** 它们保护的是上游平台和这台机器的内存
  （1 GB 树莓派上跑 100 路并发解析会直接 OOM），与身份校验无关；
  想要更放开就调 `VIDLINK_GLOBAL_CONCURRENCY`。
- **`/v1/usage` 与 `/v1/admin/*` 返回 404 而不是 403。** 它们不被注册，
  外界探测不到"这里本该有个管理接口"。

> ⚠️ **这个模式没有任何身份校验，绝不能暴露到公网。**
> 只用于本机、内网、或你完全控制的调用方。启动日志里会有一条 WARN 提醒。

## API

完整规格见 **[docs/API.md](docs/API.md)**（人读）与 **[docs/openapi.yaml](docs/openapi.yaml)**（机器读）。

| 方法 | 路径 | 鉴权 | 配额 | 说明 |
|---|---|---|---|---|
| GET | `/v1/version` | 公开 | — | 服务与接口版本 |
| GET | `/v1/health` | 公开 | — | 健康检查 + 缓存统计 |
| GET | `/v1/platforms` | 公开 | — | 平台清单与各端点系数 |
| GET | `/healthz` | 公开 | — | **存活**探针（进程活着即 200） |
| GET | `/readyz` | 公开 | — | **就绪**探针（无可用平台时 503） |
| GET | `/v1/usage` | Key | — | 自己的配额、用量、倍率（免校验模式下不存在） |
| GET | `/v1/info?url=` | Key | 0.5 / 抖音 0.75 | 元信息 + 档位列表，**无直链** |
| GET | `/v1/links?url=&quality=` | Key | 1.0 / 抖音 1.1 | **只有直链** |
| GET | `/v1/detail?url=` | Key | 1.2 / 抖音 1.5 | 元信息 + 全部档位直链 |
| POST | `/v1/batch/links` | Key | 0.75/条 | 批量直链，5~20 条，**无抖音** |
| GET | `/v1/proxy?url=` | Key | — | 流式媒体代理（默认关闭，需白名单） |
| GET/POST | `/v1/admin/accounts` | 管理员 | — | 账号列表 / 创建账号（免校验模式下不存在） |
| GET/PATCH/DELETE | `/v1/admin/accounts/{key}` | 管理员 | — | 查 / 改 / 删账号 |
| GET | `/v1/admin/stats` | 管理员 | — | 运行统计 |
| GET | `/v1/admin/quota` | 管理员 | — | 配额系数全貌（只读） |

每个响应都带 `X-Request-Id`，错误体里也有同一个值，报障时直接提供即可定位：

```
$ curl -i -H 'X-API-Key: ...' 'http://127.0.0.1:8080/v1/nope'
HTTP/1.1 404 Not Found
X-Request-Id: 91c0651a168bd9acaf6f85b2225d0c9f
{"error":{"kind":"not_found","message":"未知路径: /v1/nope","request_id":"91c0651a168bd9acaf6f85b2225d0c9f"}}
```

### 配额

```
一次调用的消耗 = 端点系数(端点, 平台) × 条数 × 账号倍率
```

- **只在实际成功时扣**：上游超时、内容不存在、限流、参数错误都不扣；批量只按成功条数扣；
- 响应头回写 `X-Quota-Consumed` / `X-Quota-Remaining`；
- 解析前会按该端点的**最贵档位**做一次上限检查（预授权），不够直接 `429`，真正扣减仍按实际平台结算；
- 账号倍率由管理员设置：`0` 不扣配额（仍记调用次数）、`0.5` 减半、`1.0` 标准、`2.0` 加倍。

配额是**用量额度**，不是余额也不是货币：服务不涉及任何支付、充值或交易，
接口里也不存在任何金额字段。遇到配额不足返回 `429`（**刻意不用 402 Payment Required**）。

数值依据与完整矩阵见 **[docs/配额倍率表.md](docs/配额倍率表.md)**，
面向用户的简版见 **[docs/配额说明-用户版.md](docs/配额说明-用户版.md)**。

### 状态码的判定规则

刻意区分三件事，便于客户端排障：

| 情况 | 状态码 |
|---|---|
| **路径**不存在（`/v1/nope`） | **404** |
| **方法**不对（`POST /v1/links`） | **405** + `Allow` 头 |
| **参数取值**不合法（缺 `url`、清晰度不存在） | **400** |
| 没有 Key / Key 无效 / 账号停用 | **403**（不区分，避免枚举 Key） |
| 同一个 Key 并发调用 | **429** `concurrency_limited`（`Retry-After: 1`） |
| 配额不足 | **429** `quota_exhausted` |
| 本地过载（解析槽位 + 排队满） | **503** `unavailable` |

### 限流与并发

三层，互不相同：

| 层 | 粒度 | 默认 | 表现 |
|---|---|---|---|
| 按 IP 请求限流 | IP | 120 RPM | `429` + `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `Retry-After` |
| 按 Key 并发 | 账号 | 1 | 仅计量端点：`429`，**不排队**，第二个请求立刻被拒 |
| 全局解析槽位 | 进程 | 10 + 队列 30 + 等 15s | 排队满/超时 → `503` |

> ⚠️ `VIDLINK_RATE_LIMIT_RPM=120` 是**每秒 2 次、允许短时突发**，
> 不是"每分钟可以一次性打 120 次"。

要点：

- `videos[0]` 即**最佳清晰度**（服务端已按 分辨率 → 帧率 → 编码兼容性 → 码率 排序）；
- `needs_mux=true` 表示 DASH 音视频分离，需要混流：
  ```bash
  ffmpeg -i video.m4s -i audio.m4s -c copy out.mp4
  ```
- `headers` 是**播放该直链时必须携带的请求头**（平台 CDN 有 Referer 防盗链）。
  浏览器里直接 `<video src>` 会 403 —— 这时用 `/v1/proxy`。

## 账号与配额管理

全新部署没有任何账号，而创建账号又需要管理员权限。服务在冷启动时自动创建
一个管理员并把 Key 打印一次（只打印一次）：

```bash
# 方式 A：让它随机生成，去日志里捞
./vidlink
# level=WARN msg="已创建初始管理员账号，请立即保存这个 Key（只显示这一次）" admin_key=vl_admin_...

# 方式 B：自己指定（容器化部署推荐，不必翻日志）
VIDLINK_ADMIN_KEY=$(openssl rand -hex 32) ./vidlink
```

拿到管理员 Key 后：

```bash
ADMIN=vl_admin_xxx

# 建一个账号：初始配额 100、标准倍率
curl -X POST -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"示例账号","quota":100}' http://127.0.0.1:8080/v1/admin/accounts
# → 响应里的 key 就是明文（只此一次），此后一律掩码

# 补 100 配额
curl -X PATCH -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"add_quota":100}' http://127.0.0.1:8080/v1/admin/accounts/<明文Key>

# 活动/内部账号：按 0.5 倍消耗
curl -X PATCH -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"multiplier":0.5}' http://127.0.0.1:8080/v1/admin/accounts/<明文Key>

# 停用 / 恢复
curl -X PATCH ... -d '{"disabled":true}'  ...
curl -X PATCH ... -d '{"disabled":false}' ...
```

账本是 **append-only JSONL**（每次写一条完整快照 + `fsync`），重启自动回放，
损坏的行会被跳过而不是让服务起不来。默认路径 `data/accounts.jsonl`。

## 配置

全部通过环境变量注入，**代码里不写死任何 Cookie 或密钥**。

| 变量 | 默认 | 说明 |
|---|---|---|
| `VL_EASE` | `false` | **免校验模式**：账户/配额/鉴权整体关闭，只留解析（见上一节） |
| `VIDLINK_ADDR` | `:8080` | 监听地址 |
| `VIDLINK_ADMIN_KEY` | 空 | 首次启动时用它创建初始管理员；空则随机生成并打印一次 |
| `VIDLINK_ACCOUNTS_PATH` | `data/accounts.jsonl` | 账本落盘路径；**留空 = 纯内存，重启即丢** |
| `VIDLINK_RATE_LIMIT_RPM` | `120` | 每 IP 每分钟请求上限；`0` = 不限 |
| `VIDLINK_PER_KEY_CONCURRENCY` | `1` | 同一个 Key 的同时请求数 |
| `VIDLINK_GLOBAL_CONCURRENCY` | `10` | 全局同时解析数 |
| `VIDLINK_QUEUE_MAX` | `30` | 全局排队上限；`0` = 不排队，满了直接 503 |
| `VIDLINK_QUEUE_WAIT_TIMEOUT` | `15s` | 最长排队等待 |
| `VIDLINK_BATCH_MIN` | `5` | 批量最少条数 |
| `VIDLINK_BATCH_MAX` | `20` | 批量最多条数 |
| `VIDLINK_CORS_ORIGINS` | `*` | 允许的跨域来源 |
| `VIDLINK_COOKIE_BILIBILI` | 空 | B 站 `SESSDATA=...`（**非必需**，匿名即可 1080P） |
| `VIDLINK_COOKIE_DOUYIN` | 空 | 抖音 `UIFID_TEMP=...`，**主链路必需**，见下文 |
| `VIDLINK_COOKIE_KUAISHOU` | 空 | 快手 Cookie（当前主链路不需要） |
| `VIDLINK_COOKIE_XIAOHONGSHU` | 空 | 小红书 Cookie（主链路不需要） |
| `VIDLINK_PROXY` | 空 | 上游 HTTP/SOCKS5 代理 |
| `VIDLINK_CACHE_TTL` | `10m` | 解析结果缓存时长 |
| `VIDLINK_NEGATIVE_TTL` | `45s` | 确定性失败的负缓存时长 |
| `VIDLINK_PARSE_TIMEOUT` | `20s` | 单次解析时间预算 |
| `VIDLINK_HTTP_TIMEOUT` | `12s` | 单次上游请求超时 |
| `VIDLINK_HTTP_RETRIES` | `2` | 上游请求重试次数 |
| `VIDLINK_BATCH_CONCURRENCY` | `3` | 批量内部的并发度 |
| `VIDLINK_UPSTREAM_RPS` | `8` | 每主机每秒上游请求上限 |
| `VIDLINK_UPSTREAM_BURST` | `16` | 每主机令牌桶突发容量 |
| `VIDLINK_MAX_IDLE_PER_HOST` | `32` | 每主机空闲连接数 |
| `VIDLINK_PROXY_ENDPOINT` | `false` | 是否开启媒体代理 |
| `VIDLINK_PROXY_ALLOW_HOSTS` | 空 | 媒体代理域名白名单（开启代理时**必填**） |
| `VIDLINK_TRUSTED_PROXY_HEADER` | 空 | 如 `X-Forwarded-For`，用于取真实客户端 IP |
| `VIDLINK_PPROF` | `false` | 开启 pprof（仅 `127.0.0.1:6060`） |

> ⚠️ **媒体代理是开放代理风险点**。默认关闭；开启时必须配置
> `VIDLINK_PROXY_ALLOW_HOSTS` 白名单，否则服务启动即报错。

## 部署

```bash
make build            # 当前平台
make build-arm64      # 树莓派（linux/arm64）
make docker           # 容器镜像

make check            # CI 入口：格式 + 静态检查 + 测试
make help             # 全部可用目标
```

容器镜像基于 `scratch`：只有静态二进制 + CA 证书，**无 shell、非 root**。
健康检查由二进制自身完成（`-healthcheck=auto`），因为 scratch 里没有 curl。

```bash
docker run -d -p 8080:8080 \
  -v vidlink-data:/app/data \
  -e VIDLINK_ADMIN_KEY="$(openssl rand -hex 32)" \
  vidlink:latest
# 抖音需要访客身份时，再把 VIDLINK_COOKIE_DOUYIN 传进来（本地铸造，见上文）
```

**务必挂载 `/app/data`**：账本在那里，不挂载的话容器一重建，所有账号与配额就没了。

## 项目结构

```
vidlink/
├── main.go                          入口：装配 + 初始管理员引导 + 优雅关闭 + 探针模式
├── Makefile                         常用开发与运维命令
├── Dockerfile                       scratch 镜像（静态二进制 + CA）
├── .github/workflows/ci.yml         格式/静态检查/测试/交叉编译/镜像
├── internal/
│   ├── core/                        领域模型 + Extractor 接口（最底层，零依赖）
│   ├── config/                      环境变量配置
│   ├── netx/                        HTTP 客户端：连接池 / 按主机限流 / 重试 / 短链展开
│   ├── cache/                       TTL 缓存 + singleflight（泛型）
│   ├── jsonx/                       容错 JSON：多路径回退 + undefined/尾逗号兼容
│   ├── webx/                        HTML 内嵌状态提取（window.X = {...}）
│   ├── urlx/                        URL 提取与归一化
│   ├── sign/
│   │   ├── sm3/                     SM3 国标哈希（纯 Go）
│   │   ├── abogus/                  抖音 a_bogus 签名
│   │   ├── secsdk/                  抖音 x-secsdk-web-signature（纯 MD5）
│   │   └── wbi/                     B 站 WBI 签名
│   ├── deps/                        共享依赖容器（打破循环依赖）
│   ├── extract/                     提取器注册表
│   │   ├── bilibili/  douyin/  kuaishou/  xiaohongshu/
│   ├── service/                     业务编排：缓存 / 合并 / 超时预算 / 平台路由
│   ├── tier/                        三档出参裁剪（info / links / detail）
│   ├── quota/                       配额消耗系数表（倍率）
│   ├── account/                     账号账本：配额 / 倍率 / 用量 + JSONL 落盘
│   ├── gate/                        两道并发闸门（按 Key + 全局）
│   └── server/                      HTTP 路由 / 中间件 / 管理面 / 流式代理
│       └── ui.html                  免校验模式的图形化解析页（内嵌进二进制）
├── tools/douyin-mint/               （仅本地，未随仓库发布）抖音访客身份铸造
└── docs/
    ├── API.md                       接口文档（人读）
    ├── openapi.yaml                 OpenAPI 3.0 接口规格
    ├── 配额倍率表.md                 系数数值、定档依据与修改流程
    ├── 配额说明-用户版.md            面向调用方的配额速查
    ├── 平台调研报告.md               四平台机制、签名、水印、风控
    └── 架构设计.md                   设计决策与取舍
```

> `tools/douyin-mint/` 与 `docs/evidence/` **不在公开仓库里**（见文末"未随仓库发布的内容"），
> 但它们留在本地：前者是 `make mint-douyin` 的实现，后者是调研报告的原始取证材料。

## 新增一个平台

只需三步，**不用改服务层和路由层**：

```go
// 1. 实现 core.Extractor
type Extractor struct{ d *deps.Deps }

func (e *Extractor) Name() core.Platform { return "myplatform" }
func (e *Extractor) Hosts() []string     { return []string{"myplatform.com"} }
func (e *Extractor) Match(u *core.URL) bool { /* 是否是可解析的作品页 */ }
func (e *Extractor) Parse(ctx context.Context, u *core.URL) (*core.Video, error) { /* ... */ }

// 2. 在 internal/extract/registry.go 的 NewRegistry 里加一行
// 3. 完事
```

4. （可选）如果它的上游成本与通用档明显不同，再去 `internal/quota/quota.go`
   的 `DefaultTable()` 加一行系数；不加就自动走通用档，有合理默认值。

## 实测状态（真实链接，非模拟）

用真实分享链接逐一验证，并**实际 Range 下载直链前 64KB** 确认可用：

| 平台 | 结果 | 直链可用性 | 说明 |
|---|---|---|---|
| **哔哩哔哩** | ✅ 完整可用 | `HTTP 206`，`ftypiso5` 真 MP4 | DASH 多清晰度；**匿名即可 1080P**（无需 Cookie）|
| **快手** | ✅ 完整可用 | `HTTP 206`，`ftypisom` 真 MP4 | 零签名零 Cookie；返回 `v4.oskwai.com` 直链 |
| **小红书** | ✅ 完整可用 | `HTTP 206`，`ftypisom` 真 MP4 | 零签名；含图文去水印改写 |
| **抖音** | ✅ 完整可用 | `HTTP 200`，`ftypisom` 真 MP4 | 需一次性铸造 `UIFID_TEMP`（见下）；返回 23 档清晰度，最高 4K |

### 抖音：需要一次性铸造访客身份

抖音的 detail 接口要求**三重签名**，三者全部是纯 Go 计算，不需要 JS 引擎或浏览器：

| 签名件 | 实现 | 说明 |
|---|---|---|
| `a_bogus` | `internal/sign/abogus` | SM3 + RC4 + 自定义 base64 |
| `x-secsdk-web-signature` | `internal/sign/secsdk` | 纯 MD5 + 公开盐；是格式约束而非密钥 |
| `timestamp` | `time.Now()` | — |

唯一算不出来的是 **`UIFID_TEMP`** —— 服务端签发给浏览器的**匿名访客标识**
（160 字符）。它**不需要登录**，但必须由浏览器或等价流程铸造一次：

```bash
# 用 Node 执行首页下发的 byted_acrawler 虚拟机；不需要浏览器
# 注意：这个脚本不在公开仓库里（见文末说明），需要在本地保留的副本中运行
tools/douyin-mint/mint-identity.sh
export VIDLINK_COOKIE_DOUYIN='UIFID_TEMP=...; ttwid=...'
```

实测：铸一次可稳定复用（同身份连发 10/10 成功，中位 304 ms），Cookie 有效期以年计。

### 两个必须知道的实测结论

1. **限流是 IP 维度的，换身份无效。** 触发限流后（约 20 线程并发 / 60 次突发），
   连真实 Chrome 发出的、带完整签名的请求也同样是空响应。轮换 Cookie 绕不过去。
2. **限流表现为 `HTTP 200 + 0 字节`，不是 4xx。** 最阴险的一种：只判断状态码
   会误判为成功。本项目将其映射为上游异常（HTTP 502）。惩罚窗口实测
   约 20 分钟后自行恢复。

详见 [`docs/平台调研报告.md`](docs/平台调研报告.md#1-抖音)。

## 合规提示

本项目仅用于技术研究与个人学习。请注意：

- **只做"选择平台已发布的干净流"，不做任何"去除水印"的图像处理**。
  平台下发的台标、以及 UP 主压进画面的水印，都不在本项目处理范围内
  （B 站画面内的强制水印属于这一类，无法去除）。
- **配额是管理员分配的用量额度，不是商品、不是余额**：本服务不提供任何
  充值、购买或支付能力，接口中不存在金额字段，也不以任何形式出售平台内容的访问权。
- 不绕过付费/会员/版权限制。遇到地区限制等错误应**原样透传**给调用方。
- 对"试看片段"要如实标注，不要把试看当完整版返回。
- 请遵守各平台的服务条款与所在地法律法规，控制请求频率。
- **`VL_EASE=true` 会关闭全部身份校验**，只适合本机或可信内网；
  把它暴露到公网等于提供一个任何人都能用的解析接口，风险自负。

## 未随仓库发布的内容

以下内容**有意排除**在公开仓库之外（已写入 `.gitignore`），它们与服务的构建、
运行、测试都无关：

| 路径 | 内容 | 为什么不发布 |
|---|---|---|
| `refs/` | 14 个第三方参考实现（约 63 MB） | 是别人的代码，不是本项目的一部分 |
| `docs/evidence/` | 原始取证笔记（成段引用他人源码并标注 `文件:行号`） | 含对第三方代码的成段摘录，且引用的仓库本身不在本仓库里 |
| `tools/douyin-mint/` | 访客身份/签名铸造脚本（下载并执行平台下发的混淆 JS 虚拟机） | 属于操作平台风控的对抗性工具，不适合公开分发 |
| `data/` | 运行期账号账本 | 含账号 Key 与配额，属于运行数据 |

因此：**克隆本仓库后，抖音的访客身份铸造步骤无法直接执行**，README 中相关
说明需要你自备等价实现；其余平台（B 站、快手、小红书）与全部服务代码不受影响。

## 许可证

[**GNU AGPL-3.0**](LICENSE) © 2026 wzmwayne

选它的理由：AGPL 比 GPL 多一条**网络服务条款**——如果你把本项目的修改版
部署成对外提供的网络服务，也必须向使用者提供修改后的完整源码。
对这类"跑在服务器上、用户只看得到接口"的项目，这是唯一能保证改动回流的选择。

简单说：

- ✅ 可以自由使用、修改、分发（包括商用），前提是保留版权声明、
  衍生作品同样以 AGPL 授权；
- ⚠️ 提供网络服务也算"分发"：必须向使用者提供源码（含你的修改）；
- ❌ 不能闭源后作为托管服务对外提供而不公开源码。

> 许可证只覆盖本仓库的代码。**不构成对任何第三方平台内容或接口的授权**，
> 使用者需自行确保其用法符合各平台服务条款与所在地法律（见上文"合规提示"）。

## 文档

- [**API 文档**](docs/API.md) — 接口说明、示例与排障
- [OpenAPI 3.0 规格](docs/openapi.yaml) — 接口契约（可直接生成客户端 SDK）
- [配额倍率表](docs/配额倍率表.md) — 每个系数是多少、为什么、怎么改
- [配额说明（用户版）](docs/配额说明-用户版.md) — 面向调用方的配额速查
- [平台调研报告](docs/平台调研报告.md) — 四平台的接口、签名、水印机制、风控与实测结论
- [架构设计](docs/架构设计.md) — 设计决策、取舍与演进路线
