# vidlink API 文档

- **版本**：`v1`（`GET /v1/version` 的 `api_version`）
- **基地址**：`http://<host>:<port>`（默认 `:8080`）
- **格式**：请求与响应一律 UTF-8 JSON（`Content-Type: application/json; charset=utf-8`）
- **计量单位**：**配额**（quota）。配额是管理员分配给账号的**用量额度**，
  与金额、支付无关。倍率说明见 [`配额倍率表.md`](配额倍率表.md)。
- **运行模式**：账户模式（默认）与**免校验模式**（`VL_EASE=true`）。
  后者没有 Key、账号、配额与管理接口，见 [§2.9](#29-免校验模式vl_easetrue)。

## 目录

1. [快速开始](#1-快速开始)
2. [通用约定](#2-通用约定)
3. [端点详解](#3-端点详解)
4. [数据模型](#4-数据模型)
5. [各平台差异](#5-各平台差异)
6. [典型场景](#6-典型场景)
7. [排障](#7-排障)
8. [速查表](#8-速查表)

---

## 1. 快速开始

### 一条链接 → 直链

```bash
curl -H "X-API-Key: $KEY" \
  "http://127.0.0.1:8080/v1/links?url=https://v.douyin.com/xxxx/"
```

```json
{
  "url": "https://v3-web.douyinvod.com/....../video.mp4",
  "backup_urls": ["https://v26-web.douyinvod.com/....../video.mp4"],
  "headers": {"Referer": "https://www.douyin.com/", "User-Agent": "..."}
}
```

### 四个计量端点怎么选

| 想干什么 | 用哪个 | 消耗（通用档 / 抖音） |
| --- | --- | --- |
| 先看看有哪些清晰度 | `GET /v1/info` | 0.5 / 0.75 |
| 只要一条能播的直链 | `GET /v1/links` | 1.0 / 1.1 |
| 元信息 + 全部档位直链 | `GET /v1/detail` | 1.2 / 1.5 |
| 5~20 条一起取直链 | `POST /v1/batch/links` | 0.75/条（不支持抖音） |

`links` 与 `detail` 都返回 `headers`——**直链必须带上这些请求头才能用**，
它们透传的是上游 CDN 的防盗链要求。

### 不想管 Key / 配额？

```bash
VL_EASE=true ./vidlink          # 免校验模式
curl 'http://127.0.0.1:8080/v1/links?url=...'   # 不带任何凭据
```

用浏览器打开 `http://127.0.0.1:8080/` 会得到一个**图形化解析页**：
填链接、选清晰度、点按钮即可，结果按直链 / 请求头 / 视频轨 / 音频轨 / 字幕 / 图集 /
原始 JSON 分区渲染，支持批量（5~20 条）与一键复制。页面由服务端内嵌，
不引用任何外部资源，做的事与 curl 完全一致。

页面还提供**浏览器内混流**：DASH 分离流（如 B 站）可以流式下载两条轨到浏览器存储
（OPFS，分片落盘，内存占用与文件大小无关），在页面里合并成一个 MP4 再保存到本地，
不需要 ffmpeg。两点与部署相关：

- 合并产物是**片段级重排**（不重新编码），只调用本服务的 `/v1/links`；
- 若某个 CDN 节点要求 Referer/UA（浏览器禁止脚本设置这两个头），
  页面会自动改走 `/v1/proxy`（免校验模式下默认开启，无需额外配置）。

### 拿到的直链怎么用

```bash
# A. 播放器/下载器直接用（把 headers 一并带上）
ffplay -headers $'Referer: https://www.bilibili.com/\r\n' "$URL"

# B. 浏览器播放：走服务端代理（需服务端开启，见 §3.11）
#    /v1/proxy?url=<urlencode(直链)>

# C. B 站的 DASH 分离流（返回 audio_url 时）需要先混流
ffmpeg -i "$VIDEO" -i "$AUDIO" -c copy out.mp4
```

---

## 2. 通用约定

### 2.1 输入的写法

`url` 参数接受三种写法，**都不需要自己补 `http://`**：

```
https://www.bilibili.com/video/BV1294y1Y7tU/     ← 完整链接
www.bilibili.com/video/BV1294y1Y7tU/             ← 从地址栏/分享面板复制出来的裸域名
7.87 复制打开抖音… https://v.douyin.com/iRxxxx/    ← 整段分享文案（自动提取链接）
```

裸域名会被补成 `https://`。判定规则是"域名 + 字母顶级域"，
所以 `BV1294y1Y7tU`、`7.87`、`v1.0` 这类内容不会被误判成域名——
它们仍然走"按 ID 解析"的老路（用 `platform` + `id` 更明确）。

### 2.2 鉴权

除公开端点外，所有请求都要带 API Key，三种传法等价：

| 方式 | 示例 |
| --- | --- |
| 请求头（推荐） | `X-API-Key: vl_xxx` |
| Bearer | `Authorization: Bearer vl_xxx` |
| 查询参数 | `?key=vl_xxx`（会进访问日志与浏览器历史，仅测试用） |

Key 由管理 Key 创建（见 §3.9）。**Key 的明文只在创建时返回一次**，
之后所有接口只返回掩码；若丢失，请删除该账号并重建。

失败一律 `403` + `kind: "forbidden"`，并且**不区分**"Key 不存在"与"Key 错误"
（避免被用来枚举有效 Key）：

```json
{"error": {"kind": "forbidden", "message": "API Key 无效", "request_id": "..."}}
```

无 Key 时额外返回 `WWW-Authenticate: Bearer realm="vidlink"`。

公开端点（不需要 Key）：`/v1/version`、`/v1/health`、`/v1/platforms`、
`/healthz`、`/readyz`。

### 2.3 配额

每次调用**成功返回**后才扣配额，响应头回写结果：

| 头 | 含义 |
| --- | --- |
| `X-Quota-Consumed` | 本次实际消耗 |
| `X-Quota-Remaining` | 该账号剩余可用配额 |

```
消耗 = 端点系数(端点, 平台) × 条数 × 账号倍率
```

- 上游超时、内容不存在、限流、参数错误：**不扣**；
- 批量：只按**成功条数**扣；
- 解析开始前会按该端点的**最贵档位**做一次上限检查，不够就 `429`；
  真正扣减仍按实际平台结算，不会多扣。

配额不足：

```json
{"error": {"kind": "quota_exhausted",
  "message": "配额已用尽：本次最多需要 1.50，当前剩余 0.20；配额由管理员分配，请联系管理员调整"}}
```

> 刻意**不用 `402 Payment Required`**：那个状态码字面就是"需要付款"。
> 配额是用量额度，`429` 语义准确。

### 2.4 错误格式

```json
{
  "error": {
    "kind": "not_found",
    "message": "bilibili: 稿件不存在或已删除",
    "request_id": "3f9a1c2b7d4e5018"
  }
}
```

| `kind` | HTTP | 含义 |
| --- | --- | --- |
| `bad_input` | 400 | 参数不合法（缺 `url`、清晰度不存在、条数超限……） |
| `unsupported` | 400 | 链接或平台不受支持 |
| `not_found` | 404 | 内容不存在 / 已删除 |
| `forbidden` | 403 | 缺少或无效 Key、账号停用、需要登录态 |
| `rate_limited` | 429 | 被按 IP 限流 |
| `concurrency_limited` | 429 | 同一个 Key 同时只允许一个**解析**请求（`Retry-After: 1`） |
| `quota_exhausted` | 429 | 配额不足（见 §2.3） |
| `timeout` | 504 | 上游超时 |
| `upstream_error` | 502 | 上游平台异常 |
| `unavailable` | 503 | 本地过载：解析槽位与排队都满了，稍后重试 |
| `method_not_allowed` | 405 | 路径存在但方法不对，响应带 `Allow` |
| `internal` | 500 | 服务端内部错误 |

### 2.5 Request ID

- 请求可带 `X-Request-Id`（≤64 字符，超长会被忽略）；
- 没带则服务端生成；
- **响应头一定回写** `X-Request-Id`，错误体里也带 `request_id`；
- 服务端日志用同一个 id。报障时提供它即可定位到那一次请求。

### 2.6 限流与并发

三层，互不相同，都要知道：

| 层 | 粒度 | 默认 | 表现 |
| --- | --- | --- | --- |
| 按 IP 请求限流 | IP | 关闭（`VIDLINK_RATE_LIMIT_RPM=0`） | `429 rate_limited` + `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `Retry-After` |
| 按 Key 并发 | 账号 | 1 | 仅**计量端点**：`429 concurrency_limited`，**不排队**，第二个请求立刻被拒 |
| 全局解析槽位 | 进程 | 10，队列 30，等 15s | 排队满/超时 → `503 unavailable` |

设计取舍：按 Key **不排队**而直接拒绝，因为队列会把"客户端以为在等结果"
变成"占着连接和内存"；全局槽位排队，因为它保护的是上游而不是公平性。
闸门只作用于四个计量端点——`/v1/usage`、`/v1/platforms`、`/v1/admin/*`、`/v1/proxy`
不占用这个槽位，因此解析进行中查用量不会被误伤。
批量的 N 条**只占用一个 Key 槽位**，但每条各自占用一个全局槽位。

### 2.7 缓存与直链时效

- 相同内容 10 分钟内命中缓存（`VIDLINK_CACHE_TTL`），命中的响应带 `"cached": true`，
  且**不会**再打上游；
- 并发请求同一内容会被合并成一次上游请求；
- 确定性失败（内容不存在、需要登录态）负缓存 45 秒；网络抖动类失败**不缓存**；
- 平台直链本身约 1~2 小时过期（B 站文档写 120 分钟），**请勿长期存储直链**。

### 2.8 状态码判定规则

- 未匹配任何路由 → `404 not_found`；
- 路径匹配但方法不对 → `405 method_not_allowed` + `Allow: ...`；
- `OPTIONS` 预检在鉴权之前处理，跨域预检不会被 Key 拦住。

---

### 2.9 免校验模式（`VL_EASE=true`）

启动时设置 `VL_EASE=true`，服务进入免校验模式：**账户体系整体关闭**，
只保留公开端点与四个解析端点。

| 行为 | 账户模式 | 免校验模式 |
| --- | --- | --- |
| 鉴权 | 需要 Key，失败 403 | **不需要任何凭据** |
| 配额 | 预授权 + 结算，回写 `X-Quota-*` | **不计量**，不回写这两个头 |
| 按 Key 并发 | 1（第二个请求 429） | 不适用 |
| 按 IP 限流 | 按配置 | **关闭** |
| `/v1/usage` | 可用 | **404** |
| `/v1/admin/*` | 可用（凭据是固定的管理 Key） | **404** |
| `/admin`（管理面板）、`/`（解析页） | 视 `VL_WEBUI`（默认关） | **404** / 视 `VL_WEBUI`（默认开） |
| `/v1/platforms` | 含 `rates` / `unit` | 不含 `rates` / `unit` |
| 解析端点 | 可用（需 Key） | **可用（无凭据）** |
| 根路径 `/` | 视 `VL_WEBUI`（默认纯文本导航页，需 Key） | 视 `VL_WEBUI`（默认**图形化解析页**） |
| 全局解析槽位 | 10 + 队列 30 | **保留**（资源保护，非校验） |
| 批量 `cost` 字段 | 实际消耗 | 恒为 `0` |

`/v1/health` 与 `/v1/platforms` 都会返回 `"mode": "ease" | "account"`，
客户端可以据此决定要不要带 Key；`/v1/health` 另有 `"webui"` 告知根路径
当前是不是图形化页面。

> ⚠️ 这个模式没有任何身份校验，**不要暴露到公网**。它适合本机脚本、
> 内网服务、或你完全控制调用方的场景。

## 3. 端点详解

### 3.1 `GET /v1/version` · 公开

```json
{"version": "v0.88", "api_version": "v1", "platform": "linux/arm64"}
```

### 3.2 `GET /v1/health` · 公开

```json
{
  "status": "ok", "version": "v0.88", "api_version": "v1",
  "uptime_sec": 3821, "requests": 10422, "errors": 37,
  "cache": {"entries": 128, "hits": 9014, "misses": 1408, "coalesced": 52, "loads": 1408}
}
```

### 3.3 `GET /v1/platforms` · 公开 · 不消耗配额

```json
{
  "platforms": [{
    "name": "bilibili",
    "hosts": ["bilibili.com", "b23.tv"],
    "supports_id": true,
    "has_cookie": false,
    "rates": {"info": 0.5, "links": 1.0, "detail": 1.2, "batch_links": 0.75},
    "batch": true,
    "batch_reason": ""
  }],
  "unit": "配额",
  "limits": {"batch_min": 5, "batch_max": 20}
}
```

抖音的 `batch` 为 `false`，`batch_reason` 给出可读原因。

### 3.4 `GET /healthz` · 公开

存活探针：`{"status": "alive"}`，永远 200，带 `Cache-Control: no-store`。

### 3.5 `GET /readyz` · 公开

就绪探针：注册了平台提取器时 `{"status":"ready","platforms":4}`，
否则 `503` + `{"status":"not_ready","reason":"没有注册任何平台提取器"}`。

### 3.6 `GET /v1/usage` · 需 Key · 不消耗配额 · 免校验模式下不存在

> `VL_EASE=true` 时该端点不被注册，返回 `404`。

自己账号的用量与系数，用于自查"为什么扣了这么多"：

```json
{
  "name": "示例账号",
  "unit": "配额",
  "quota": 87.3,          // 剩余可用配额
  "used": 12.7,           // 累计消耗
  "calls": 14,            // 累计调用次数
  "multiplier": 1.0,      // 账号倍率
  "rates": {"douyin": {"info": 0.75, "links": 1.1, "detail": 1.5},
            "bilibili": {"info": 0.5, "links": 1.0, "detail": 1.2, "batch_links": 0.75}},
  "limits": {"per_key_concurrency": 1, "global_concurrency": 10,
             "batch_min": 5, "batch_max": 20}
}
```

### 3.7 三个计量端点

三者共用输入参数：

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `url` | 二选一 | 分享链接或整段分享文案（自动从文本里提取链接）；**可以不带 `http://`**，如 `www.bilibili.com/video/BV1xx` |
| `platform` + `id` | 二选一 | 已知平台与内容 ID 时最省一次跳转 |
| `quality` | 否 | 仅 `links` / 批量用：`1080` / `720P` / `qn:80`；缺省取推荐档 |

#### `GET /v1/info` · 0.5（抖音 0.75）

只给元信息与**可选档位列表**，不含任何直链：

```json
{
  "platform": "bilibili", "id": "BV1xx411c7mD",
  "title": "标题", "desc": "...", "cover": "https://...",
  "author": {"id": "123", "name": "UP 主", "avatar": "https://..."},
  "stats": {"view": 123456, "like": 2345, "danmaku": 678, "duration": 213},
  "source_url": "https://www.bilibili.com/video/BV1xx411c7mD",
  "qualities": [
    {"height": 1080, "label": "1080P", "quality_id": 80, "codecs": ["avc1", "hev1"]},
    {"height": 480, "label": "480P", "quality_id": 32, "codecs": ["avc1"]}
  ],
  "warning": "...", "cached": true
}
```

#### `GET /v1/links` · 1.0（抖音 1.1）

**只有直链**，不含标题作者等元信息——那是 `detail` 的内容：

```json
{
  "url": "https://.../video.m4s",
  "backup_urls": ["https://.../video.m4s"],
  "headers": {"Referer": "https://www.bilibili.com/", "User-Agent": "..."},
  "audio_url": "https://.../audio.m4s",
  "needs_mux": true
}
```

- `audio_url` + `needs_mux`：DASH 分离流，需要自己混流；
- `backup_urls`：CDN 容灾，第一条失败时可以换；
- `quality` 不存在 → `400 bad_input`，消息里会列出可用档位，**且不扣配额**。

#### `GET /v1/detail` · 1.2（抖音 1.5）

`info` 的全部字段 **+** 全部档位直链：

```json
{
  "platform": "bilibili", "id": "...", "title": "...", "author": {...},
  "qualities": [...],
  "videos": [{"url": "...", "quality": "1080P", "height": 1080, "codec": "avc1",
              "bandwidth": 1234567, "headers": {...}}],
  "audios": [{"url": "...", "bandwidth": 132000, "codec": "mp4a"}],
  "needs_mux": true,
  "music": {"url": "..."},
  "subtitles": [{"lang": "zh-CN", "name": "中文（自动）", "url": "...", "format": "json"}],
  "best_video_url": "...", "best_video_headers": {...}, "best_audio_url": "..."
}
```

`best_video_url` / `best_audio_url` 是便捷字段：不想理解 DASH 结构时直接用它们。

### 3.8 `POST /v1/batch/links` · 0.75/条 · 不支持抖音

请求体（`urls` 与 `text` 至少给一个，`text` 按行拆分）：

```json
{
  "urls": ["https://v.douyin.com/a/", "https://www.bilibili.com/video/BV1xx"],
  "text": "分享文案第一行\n分享文案第二行",
  "quality": "1080"
}
```

约束：**5 ≤ 条数 ≤ 20**（`VIDLINK_BATCH_MIN` / `VIDLINK_BATCH_MAX`）。
少于 5 条会被拒并提示改用 `GET /v1/links`。

响应：

```json
{
  "total": 6, "success": 5, "failed": 1, "cost": 3.75,
  "results": [
    {"input": "https://...", "links": {"url": "...", "headers": {...}}},
    {"input": "https://v.douyin.com/a/",
     "error": {"kind": "unsupported", "message": "抖音按 IP 维度限流，……"}}
  ]
}
```

- 单条失败不影响其它条；
- `cost` 只统计成功条数（`failed` 的那些不消耗配额）；
- `cost` 已包含账号倍率，与响应头 `X-Quota-Consumed` 一致；
- 抖音在**解析前**就被挡掉（纯本地平台判定），不浪费上游请求与 IP 风控额度。

### 3.9 管理端点 `/v1/admin/*` · 需固定管理 Key · 免校验模式下不存在

> `VL_EASE=true` 时管理面整体不注册，所有 `/v1/admin/*` 返回 `404`。

**管理凭据是一个固定的 Key**，来自环境变量或 `.vl` 里的 `VIDLINK_ADMIN_KEY`；
它不是账本里的账号，账号也没有任何权限位：

| 情况 | 结果 |
| --- | --- |
| 请求里的 Key == `VIDLINK_ADMIN_KEY` | 放行 |
| 是账本里的账号 Key（哪怕配额很高） | `403 forbidden` |
| 没有配置 `VIDLINK_ADMIN_KEY` | `403 forbidden`，message 里点名该配置项 |
| 传 Key 的方式 | 与业务端点一致：`X-API-Key` 头 / `Authorization: Bearer` / `?key=` |

由此推出的两条性质：

- 管理 Key **不能**用来解析视频（它不在账本里，计量端点会回 `403`）；
- 账本里**不存在**"管理员账号"这种东西，`{"admin":true}` 这类字段会被
  `400` 拒绝（未知字段不静默忽略）。

所有响应里的账号 Key 都是掩码（创建那一次除外）；
每个账号还有一个**公开句柄** `id`（`acc_` + Key 的 SHA-256 截断），
可以拿它代替明文 Key 出现在路径里（管理面板就是这么做的）。

#### `GET /v1/admin/accounts`

```json
{
  "accounts": [{
    "id": "acc_1f2e3d4c5b6a7980", "key": "vl_a********3f7c", "name": "示例账号",
    "quota": 87.3, "multiplier": 1.0, "used": 12.7, "calls": 14,
    "disabled": false, "created_at": "2026-01-02T03:04:05Z",
    "updated_at": "2026-01-02T03:04:05Z", "note": "备注"
  }],
  "stats": {"accounts": 3, "active": 3, "used_total": 120.5,
            "calls_total": 96, "quota_total": 2500}
}
```

#### `POST /v1/admin/accounts`

```json
{"name": "新账号", "quota": 100, "multiplier": 1.0, "note": "备注"}
```

请求体里出现未知字段（例如老接口的 `admin`）会返回 `400`，而不是被静默忽略。

- 不填 `key` 时自动生成 256 位随机 Key；
- `quota` = 初始配额；`multiplier` 缺省为 `1.0`（**不是 0**，0 是"不扣配额"）；
- 响应 `201`：

```json
{"account": {"key": "vl_a********3f7c", "...": "..."},
 "key": "vl_<明文>",
 "notice": "请立即保存这个 Key，它只会出现这一次"}
```

#### `GET /v1/admin/accounts/{key|id}`

查看单个账号（掩码 Key + 句柄）。路径参数可以是**明文 Key**，
也可以是列表里的**句柄 `id`**——后者让管理面板不必接触明文 Key。
掩码 Key（`vl_a********3f7c`）**不能**当路径参数用，会得到 `404`。

#### `PATCH /v1/admin/accounts/{key}`

局部修改，字段全部可选；`quota`（设为某值）与 `add_quota`（在现值上加减）
不能同时使用：

```json
{"name": "改名", "note": "备注", "add_quota": 50, "multiplier": 0.5, "disabled": false}
```

常用组合：

| 目的 | 请求体 |
| --- | --- |
| 补充配额 | `{"add_quota": 100}` |
| 设为固定值 | `{"quota": 500}` |
| 给某账号减半 | `{"multiplier": 0.5}` |
| 内部账号不扣配额 | `{"multiplier": 0}` |
| 停用 | `{"disabled": true}` |

#### `DELETE /v1/admin/accounts/{key}`

删除账号（写删除墓碑，重启不会复活）。路径参数同样可以用明文 Key 或句柄 `id`。

删掉任何账号（包括唯一那个）都不会影响管理面本身——管理 Key 不在账本里。

#### `GET /v1/admin/stats`

```json
{
  "accounts": {"accounts": 3, "active": 3, "used_total": 120.5,
               "calls_total": 96, "quota_total": 2500},
  "gate": {"global_in_use": 2, "global_limit": 10, "queue_len": 0, "waiting": 0},
  "cache": {"entries": 128, "hits": 9014, "misses": 1408, "coalesced": 52, "loads": 1408},
  "traffic": {"requests": 10422, "errors": 37, "uptime_sec": 3821}
}
```

#### `GET /v1/admin/quota` · 不消耗配额

配额系数全貌（只读）：计量公式、各端点说明、预授权上限、各平台系数。
改系数需要改代码并重新部署——系数是**共享参数**，影响所有账号，
见 [`配额倍率表.md`](配额倍率表.md) 的"怎么改"。

### 3.10 运维端点

| 路径 | 说明 |
| --- | --- |
| `/healthz` | 容器健康检查（`-healthcheck=auto` 也用这个） |
| `/readyz` | 就绪探针 |
| `/` | 根路径。开启 `VL_WEBUI` 时返回**图形化解析页**（`text/html`，内嵌、无外部资源）：免校验模式下直接可用；账户模式下页面本身公开，但**页内所有数据请求都要填 API Key**（页内可查 `GET /v1/usage` 的配额、用量与系数）。未开启时返回纯文本导航页（账户模式需 Key） |
| `/debug/pprof/*` | 仅 `VIDLINK_ENABLE_PPROF=true` 时开启，且只监听 `127.0.0.1:6060` |

### 3.11 `GET /v1/proxy` · 需 Key · 默认关闭 · 不消耗配额

流式代理媒体文件，**支持 `Range`**（可拖动进度条），用于浏览器播放被防盗链限制的直链。

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `url` | 是 | 媒体地址（http/https 绝对地址） |
| `filename` | 否 | 下载文件名 → `Content-Disposition: attachment`（中文按 RFC 2231 编码） |
| `type` | 否 | 覆盖上游 `Content-Type`（CDN 对 `.m4s` 常回 `application/octet-stream`）；只放行媒体类型与 `application/octet-stream` |
| `referer` | 否 | 覆盖默认 Referer |
| `ua` | 否 | 覆盖默认 User-Agent |

`filename` 会做消毒：去掉控制字符、引号、路径分隔符并限长 120 字符。
这不是洁癖——它来自查询参数并要写进响应头，不消毒就等于开了响应头注入与
目录穿越两个口子。

**为什么需要 `filename`**：直链最后一段是 CDN 的资源 ID，浏览器按它命名；
经代理下载时更是只能得到一个叫 `proxy`、没有后缀的文件。带上这个参数后，
配合内嵌页面就是统一的命名：

```
<标题>VL<类型><清晰度/音频码率>.<后缀>
```

| 类型 | 含义 | 例子 |
| --- | --- | --- |
| `前端混合` | 页面里把 DASH 音视频合并后的产物 | `歌曲MV《我不曾忘记》VL前端混合1080P.mp4` |
| `原生混合` | 平台给的就是音视频一体的文件（无 `audio_url`） | `某视频VL原生混合720P.mp4` |
| `视频` | 分离流里的视频轨 | `某视频VL视频1080P.mp4` |
| `音频` | 分离流里的音频轨/背景音乐 | `歌曲MV《我不曾忘记》VL音频192K.m4a` |

开启条件：

```bash
VIDLINK_PROXY_ENDPOINT=true                    # 免校验模式下默认就是 true
VIDLINK_PROXY_ALLOW_HOSTS=upos-sz-mirrorcos.bilivideo.com,https://upos-sz-mirror08c.bilivideo.com/
```

- **白名单留空表示放行任意 http/https 目标**（想收紧就填域名后缀，多个用逗号分隔）；
- 填了白名单时，不在其中的域名 → `403 forbidden`；
- 条目会被**规范化**成裸域名后缀：剥掉 scheme、userinfo、端口、路径与开头的 `*.`，
  转小写、去首尾点、去重。所以 `*.bilivideo.com`、`https://upos-sz-mirror08c.bilivideo.com/`
  与 `upos-sz-mirror08c.bilivideo.com` 是同一个后缀；
- 匹配按 **DNS 标签后缀**：`bilivideo.com` 命中 `upos-sz-mirror08c.bilivideo.com`，
  但不命中 `notbilivideo.com`、也不命中 `bilivideo.com.evil.cn`；
- **会被丢弃的条目**：`*`、单个标签（`com`、`localhost`）、含非法字符或超长（单段 >63、
  总长 >253）的条目。丢弃不影响服务启动，但启动日志会列出被忽略的原文——
  白名单"悄悄只生效一半"是最坑人的状态；
- 补充校验：`url` 必须是 http/https 绝对地址且**不能带用户名密码**（≤4096 字节）；
  `referer` 必须是 http/https 绝对地址；`ua` 会去掉控制字符并限长。

**配额**：代理是唯一不按"端点 × 平台"计费的出口，它按**传输体积**算：

```
实扣 = 传输体积(MiB) × 1 × 账号倍率          # 不乘平台系数
```

- 上游声明了长度（`Content-Length`；Range 请求时它就是这一段的长度）→ **先扣后传**：
  配额不足直接 `429 quota_exhausted`，一个字节都不发，`X-Quota-*` 如实回写；
- 长度未知（chunked）→ 按账户余额折算字节上限，边传边限，传完按实际字节补扣
  （这条路径来不及回写 `X-Quota-*`）；
- `HEAD` 不产生响应体，**不计费**；
- 提前中断**不退**（按声明长度计费是"这次占用了多少出口带宽"的度量）；
- 计费精度 0.0001 配额（约 105 字节）；
- `GET /v1/usage` 的 `proxy` 字段与 `GET /v1/admin/quota` 的 `proxy` 字段都会给出这条口径；
- 倍率为 `0` 的账号（免费账号）依然是 0 配额，但调用次数照常累计。
- **默认值跟随运行模式**：免校验模式默认开启（浏览器内混流遇到要求 Referer/UA 的
  CDN 节点时要靠它），账户模式默认关闭；显式写 `VIDLINK_PROXY_ENDPOINT=false` 一律关掉；
- 账户模式下它需要 Key；免校验模式下它是公开的——**这时它就是一个开放代理，
  别把端口暴露到公网**（启动日志会就此给出警告）。

---

## 4. 数据模型

### 4.1 `Info`（`/v1/info`，也是 `/v1/detail` 的前半部分）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `platform` | string | `douyin` / `bilibili` / `kuaishou` / `xiaohongshu` |
| `id` | string | 平台内内容 ID |
| `title` | string | 标题 |
| `desc` | string | 描述（可能为空） |
| `cover` | string | 封面 |
| `author` | object | `{id, name, avatar}` |
| `stats` | object | `{view, like, comment, collect, share, danmaku, duration}` |
| `source_url` | string | 规范化后的原始页面地址 |
| `qualities` | array | **可选档位描述，不含直链**：`{height, label, quality_id, codecs}` |
| `images` | array | 图集 / 实况照片：`{url, live_photo_url, width, height}` |
| `warning` | string | 降级提示，例如"只拿到 480P（该稿件需要登录态）" |
| `cached` | bool | 是否命中缓存 |

### 4.2 `Links`（`/v1/links`，批量里每条也是它）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `url` | string | 视频直链 |
| `backup_urls` | array | 备用直链（CDN 容灾） |
| `headers` | object | **必须透传**的请求头（防盗链） |
| `audio_url` | string | DASH 分离流的音频直链 |
| `needs_mux` | bool | 是否需要音视频混流 |

### 4.3 `Detail`（`/v1/detail`）

`Info` 全部字段，外加：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `videos` | array | 全部视频轨（按推荐度排序） |
| `audios` | array | 全部音频轨 |
| `needs_mux` | bool | 是否需要混流 |
| `music` | object | 背景音乐（部分平台有） |
| `subtitles` | array | `{lang, name, url, format}` |
| `best_video_url` / `best_video_headers` | string / object | 推荐视频轨及其请求头 |
| `best_audio_url` | string | 推荐音频轨 |

### 4.4 `Stream`

| 字段 | 说明 |
| --- | --- |
| `url` / `backup_urls` / `headers` | 同上 |
| `quality` / `quality_id` | 档位标签与平台原始档位码 |
| `codec` / `mime_type` | 编码（`avc1` / `hev1` / `av01` / `mp4a`） |
| `width` / `height` | 分辨率 |
| `frame_rate` / `bandwidth` / `size` / `duration` | 帧率、码率、字节数、时长 |

### 4.5 `Account`

| 字段 | 说明 |
| --- | --- |
| `id` | 公开句柄 `acc_…`（Key 的哈希截断，可用于路径参数） |
| `key` | API Key（除创建外一律掩码 `vl_a********3f7c`） |
| `name` / `note` | 名称与备注 |
| `quota` | **剩余可用配额** |
| `used` | 累计消耗（已按账号倍率折算） |
| `calls` | 累计调用次数（含倍率为 0 的调用） |
| `multiplier` | 账号倍率 |
| `disabled` | 是否停用 |
| `created_at` / `updated_at` | 时间戳（RFC3339） |

---

## 5. 各平台差异

| 平台 | 按 ID 解析 | 批量 | 常见限制 |
| --- | --- | --- | --- |
| 哔哩哔哩 | 支持 | 支持 | 匿名可拿 1080P；**画面内水印无法去除**；DASH 分离流需混流 |
| 抖音 | 支持 | **不支持** | IP 维度限流，高频后 HTTP 200 但**响应体 0 字节**，恢复约 20 分钟 |
| 快手 | 支持 | 支持 | 部分内容需要 Cookie |
| 小红书 | 支持 | 支持 | 图集内容返回 `images` 而非视频轨 |

### 哔哩哔哩

- 直链带 `Referer` 防盗链要求，必须透传 `headers`；
- 视频与音频分离，`needs_mux: true` 时要先混流；
- 直链有效期约 2 小时；
- **画面内的强制水印无法去除**（平台侧烧进画面，业界普遍如此）。

### 抖音

- **无法用批量**：批量会瞬间打出多个请求，容易触发 IP 维度风控；
- 限流表现是 HTTP 200 + 0 字节，不是错误码——服务端会把它翻译成
  `upstream_error`；
- 换 IP 是唯一有效的恢复手段，换 Key 无效（限流按 IP 而非账号）；
- 拿到的 `play_addr` 是无水印源，`download_addr` 带水印——本服务只用前者。

### 快手 / 小红书

- 多为单文件直链（`needs_mux` 为 `false`）；
- 小红书部分内容带 `images`（图集），`videos` 可能为空。

---

## 6. 典型场景

### 6.1 下载一个 B 站视频（含混流）

```bash
KEY=vl_xxx
j=$(curl -s -H "X-API-Key: $KEY" \
  "http://127.0.0.1:8080/v1/links?url=https://www.bilibili.com/video/BV1xx411c7mD&quality=1080")

v=$(echo "$j" | python3 -c 'import sys,json;print(json.load(sys.stdin)["url"])')
a=$(echo "$j" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("audio_url",""))')

curl -H "Referer: https://www.bilibili.com/" -H "User-Agent: Mozilla/5.0" -o v.m4s "$v"
curl -H "Referer: https://www.bilibili.com/" -H "User-Agent: Mozilla/5.0" -o a.m4s "$a"
ffmpeg -i v.m4s -i a.m4s -c copy out.mp4
```

### 6.2 挑一条"树莓派能播"的流

```bash
curl -s -H "X-API-Key: $KEY" "http://127.0.0.1:8080/v1/detail?url=$URL" |
python3 -c '
import sys, json
d = json.load(sys.stdin)
# 优先 H.264（ARM 硬解支持最好），再取最高的那一条
ok = [s for s in d["videos"] if s.get("codec","").startswith("avc1")] or d["videos"]
best = max(ok, key=lambda s: s.get("height", 0))
print(best["url"]); print(best.get("headers"))'
```

### 6.3 批量取直链并逐条处理

```bash
curl -s -X POST -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"urls":["https://v.douyin.com/a/","https://www.bilibili.com/video/BV1xx",
               "https://v.kuaishou.com/b/","https://www.xiaohongshu.com/explore/c",
               "https://b23.tv/d/"],"quality":"720"}' \
  http://127.0.0.1:8080/v1/batch/links |
python3 -c '
import sys, json
r = json.load(sys.stdin)
print("成功", r["success"], "失败", r["failed"], "消耗", r["cost"])
for it in r["results"]:
    if "links" in it: print(it["input"], "->", it["links"]["url"])
    else:             print(it["input"], "!!", it["error"]["kind"], it["error"]["message"])'
```

### 6.4 观察配额消耗

```bash
curl -s -D- -o /dev/null -H "X-API-Key: $KEY" \
  "http://127.0.0.1:8080/v1/links?url=$URL" | grep -i x-quota
# X-Quota-Consumed: 1
# X-Quota-Remaining: 99

curl -s -H "X-API-Key: $KEY" http://127.0.0.1:8080/v1/usage
```

### 6.5 管理：补配额、给活动账号减半

```bash
# 补 100 配额
curl -s -X PATCH -H "X-API-Key: $ADMIN" -H "Content-Type: application/json" \
  -d '{"add_quota":100}' http://127.0.0.1:8080/v1/admin/accounts/vl_xxx

# 该账号按 0.5 倍消耗（活动 / 内部）
curl -s -X PATCH -H "X-API-Key: $ADMIN" -H "Content-Type: application/json" \
  -d '{"multiplier":0.5}' http://127.0.0.1:8080/v1/admin/accounts/vl_xxx
```

---

## 7. 排障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `403 forbidden` | 没带 Key / Key 错 / 账号停用 / 内容需要登录态 | 检查 Key，或让管理员看账号状态 |
| `429 concurrency_limited` | 同一个 Key 并发调用 | **串行**调用；或申请更多 Key |
| `429 quota_exhausted` | 配额用尽 | 让管理员补配额，或调低 `multiplier` |
| `503 unavailable` | 解析槽位 + 排队都满 | 降低并发，稍后重试 |
| `502 upstream_error` | 上游异常（抖音限流也走这里） | 换 IP、降低频率；隔 20 分钟再试 |
| `504 timeout` | 上游超时 | 重试；持续出现请报障 |
| 直链 403 | 没带 `headers` | 透传响应里的 `headers` |
| 直链能下但不能播 | DASH 分离流 | 看 `needs_mux`，用 `audio_url` 混流 |
| 只有 480P（B 站） | 该稿件确实需要登录态 | 看 `warning` 字段 |

### 报障时请提供

1. `X-Request-Id`（响应头或错误体里的 `request_id`）；
2. 请求的完整 URL（Key 请打码）；
3. 响应体与状态码；
4. 时间点（便于对齐日志）。

### 抖音特别说明

抖音的限流不返回错误码，而是 HTTP 200 + **响应体 0 字节**。
本服务把它识别为上游异常（`502`）而不是"内容不存在"，
所以看到 `502` 时先等 20 分钟、或换出口 IP，不用怀疑链接。

---

## 8. 速查表

| 方法 | 路径 | 鉴权 | 配额 |
| --- | --- | --- | --- |
| GET | `/v1/version` | 公开 | — |
| GET | `/v1/health` | 公开 | — |
| GET | `/v1/platforms` | 公开 | — |
| GET | `/healthz` | 公开 | — |
| GET | `/readyz` | 公开 | — |
| GET | `/v1/usage` | Key | — |
| GET | `/v1/info?url=` | Key | 0.5 / 抖音 0.75 |
| GET | `/v1/links?url=&quality=` | Key | 1.0 / 抖音 1.1 |
| GET | `/v1/detail?url=` | Key | 1.2 / 抖音 1.5 |
| POST | `/v1/batch/links` | Key | 0.75/条，5~20 条，无抖音 |
| GET | `/v1/proxy?url=` | Key | 1/MiB × 账号倍率（默认关闭） |
| GET | `/admin` | 公开（页面壳，数据要管理 Key） | —（需 `VL_WEBUI` 且非 ease） |
| GET | `/v1/admin/accounts` | 管理 Key | — |
| POST | `/v1/admin/accounts` | 管理 Key | — |
| GET | `/v1/admin/accounts/{key\|id}` | 管理 Key | — |
| PATCH | `/v1/admin/accounts/{key\|id}` | 管理 Key | — |
| DELETE | `/v1/admin/accounts/{key\|id}` | 管理 Key | — |
| GET | `/v1/admin/stats` | 管理 Key | — |
| GET | `/v1/admin/quota` | 管理 Key | — |

其他文档：

- 机器可读规格：[`openapi.yaml`](openapi.yaml)
- 配额倍率说明：[`配额倍率表.md`](配额倍率表.md)
- 架构与取舍：[`架构设计.md`](架构设计.md)
- 平台调研（实测数据）：[`平台调研报告.md`](平台调研报告.md)
