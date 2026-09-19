# vidlink API 文档

> 本文是**唯一的人读接口文档**：所有端点、请求/响应示例、错误码都在这里。
> 机器可读规格是 [`openapi.yaml`](openapi.yaml)，计费倍率见 [`配额倍率表.md`](配额倍率表.md)。
> 仓库：<https://github.com/wzmwayne/vidlink>（AGPL-3.0）

## 官方测试实例

| | |
| --- | --- |
| 地址 | **<https://vl.wzml.cc.cd>** |
| 凭据 | 公共 Key `vl_public`（免注册、每 IP 每日配额，见 [§3.8a](#38a-公共-keyvl_public--免注册试用)） |
| 说明 | 它跑在一台树莓派上，可能限流、随时变动或下线；**正式使用请自部署**（单文件二进制约 7 MB、常驻内存约 8 MB） |

**本文所有路径都是相对路径**，示例统一用 `$BASE` 变量——把它换成本文运行的服务地址即可：

```bash
# 官方测试实例
export BASE=https://vl.wzml.cc.cd
# 或者：自部署（默认监听 :8080）
export BASE=http://127.0.0.1:8080

export KEY=vl_xxx        # 你的账号 Key（没有就用公共 Key：export KEY=vl_public）
export ADMIN=vl_admin_xxx # 管理 Key（服务端 VIDLINK_ADMIN_KEY 配的那个）
```

- **版本**：`v1`（`GET /v1/version` 的 `api_version`）
- **格式**：请求与响应一律 UTF-8 JSON（`Content-Type: application/json; charset=utf-8`）
- **计量单位**：**配额**（quota）。配额是管理员分配给账号的**用量额度**，
  与金额、支付无关。倍率说明见 [`配额倍率表.md`](配额倍率表.md)；
  倍率可以在运行期用管理接口改（[§3.9.5](#395-putdelete-v1adminquota--改计费倍率)），
  所以**以 `/v1/usage` 返回的实时值为准**。
- **运行模式**：账户模式（默认）与**免校验模式**（`VL_EASE=true`）。
  后者没有 Key、账号、配额与管理接口，见 [§2.9](#29-免校验模式vl_easetrue)。
- **页面**：`$BASE/` 图形化解析页，`$BASE/admin` 管理面板（填管理 Key）。

## 目录

1. [快速开始](#1-快速开始)
2. [通用约定](#2-通用约定)
3. [端点详解](#3-端点详解)
   - 含签名凭据（`/v1/sign`）、公共 Key、每日签到、配额流水、管理面（账号 / 统计 / **计费倍率可编辑** / 流水 / 管理签名）与媒体代理
4. [数据模型](#4-数据模型)
5. [各平台差异](#5-各平台差异)
6. [典型场景](#6-典型场景)
7. [排障](#7-排障)
8. [速查表](#8-速查表)

---

## 1. 快速开始

### 一条链接 → 直链

```bash
# 推荐的传法：现算一张签名放进请求头（§2.2）。下面这条用仓库里的示例脚本，
# 等价于手写 HMAC-SHA256；临时试用也可以直接 -H "X-API-Key: vl_public"。
curl -H "X-API-Key: $(sh examples/sign.sh "$KEY")" \
  "$BASE/v1/links?url=https://v.douyin.com/xxxx/"
```

```json
{
  "url": "https://v3-web.douyinvod.com/....../video.mp4",
  "backup_urls": ["https://v26-web.douyinvod.com/....../video.mp4"],
  "headers": {"Referer": "https://www.douyin.com/", "User-Agent": "..."}
}
```

更完整的客户端示例（Python / Node / Shell 三份）见 §2.2 末的
[现成示例代码](#现成示例代码可直接跑仓库里就有的六个文件)。

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
curl '$BASE/v1/links?url=...'   # 不带任何凭据
```

用浏览器打开 `$BASE/` 会得到一个**图形化解析页**：
填链接、选清晰度、点按钮即可，结果按直链 / 请求头 / 视频轨 / 音频轨 / 字幕 / 图集 /
原始 JSON 分区渲染，支持批量（5~20 条）与一键复制。页面由服务端内嵌，
不引用任何外部资源，做的事与 curl 完全一致。

页面还提供**浏览器内混流**：DASH 分离流（如 B 站）可以流式下载两条轨到浏览器存储
（OPFS，分片落盘，内存占用与文件大小无关），在页面里合并成一个 MP4 再保存到本地，
不需要 ffmpeg。这些都与部署相关：

- 合并产物是**片段级重排**（不重新编码），只调用本服务的 `/v1/links`；
- 下载通道由界面上的复选框决定，**不做自动兜底**（自动兜底会让"这次用掉多少服务端带宽
  与配额"不可预测）：勾上就走 `/v1/proxy`，不勾则直连，失败时按 `backup_urls` 换镜像再试；
- 产物写在浏览器存储（OPFS）里，合并前会申请**持久化存储**（`navigator.storage.persist`，
  需用户手势，Firefox 会弹一次权限），避免大文件被当 best-effort 清理。
  **配额上限由浏览器决定**（Firefox 每个源约 10 GiB、Chrome 按磁盘比例），本服务不设上限；
- HLS(TS) 分片合并默认 **8** 路并发下载分片，界面可手动调整（1~32）；
- TS 合并产物现代浏览器一般能直接在页面里预览（`video/mp2t`），播不了再用 VLC / mpv / ffmpeg；
- 合并完可以点「**转成 MP4（不重编码）**」：页面里自己解 MPEG-TS（PAT/PMT/PES、
  Annex-B→AVCC、ADTS→裸 AAC、SPS 解宽高）并写 MP4 的 sample 表（`stts/ctts/stsz/co64/stss`），
  只搬字节，不重编码。只支持 **H.264 + AAC**；HEVC / AC-3 / MP2 线路会明确拒绝并保留 `.ts`。

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

除公开端点外，所有请求都要带凭据。**推荐的传法是签名**：它只有几十秒有效期，
即便泄漏（请求日志、截屏、抓包、聊天记录、浏览器历史）也换不来长期访问；
明文 Key 是长期有效的，只建议在本地临时试一下时用，而且**只能走请求头**。

| 传法 | 示例 | 有效期 | 建议 |
| --- | --- | --- | --- |
| 签名 + 请求头 | `-H "X-API-Key: acc_<句柄>.<时间戳>.<签名>"` | 约 30 秒 | ✅ **推荐**（vidlink 自己的两个页面就是这么发的） |
| 签名 + URL | `?key=acc_<句柄>.<时间戳>.<签名>` | 约 30 秒 | ✅ **推荐**（`<a>`/`<video>`/原生下载加不了请求头，这是唯一可行的传法） |
| 明文 Key + 请求头 | `-H "X-API-Key: vl_xxx"` | 长期 | ⚠️ 简单场景、本地调试 |
| 明文 Key + Bearer | `-H "Authorization: Bearer vl_xxx"` | 长期 | ⚠️ 同上 |
| 明文 Key + URL | `?key=<明文 Key>` | — | ❌ **会被拒**（`403 key_format`） |

换句话说：**凭据进 URL 就必须是签名**；请求头里两种都收，按形态识别。
服务端与两个内嵌页面都不依赖明文 Key 的传递。

#### 签名凭据怎么算

```
凭据 = 句柄 . 时间戳 . 签名
句柄 = "acc_" + hex(SHA-256(Key))[:16]           （16 位小写十六进制）
时间戳 = unix 秒，写成 8 位小写十六进制（如 68a3e658）
签名 = hex(HMAC-SHA256(Key, "句柄|时间戳hex"))    完整 64 位，不截断
```

例（`Key = vl_demo_key`，`时间戳 = 0x68a3e658`）：

```
acc_17090c89a1ca6ee3.68a3e658.35c1daf1f0dfefd2bb15906d25ff8e6e5dfaf2ce9757749b7ccbcb0c4a2a46c9
```

服务器怎么校验（**不需要反推、也不需要遍历**）：

1. 读出票面的时间戳（明文的），算 `|服务器时间 − 时间戳| ≤ 30` 秒；
2. 用该句柄对应账号的 Key 把 `句柄|时间戳hex` 重新算一遍 HMAC 比对（一次正向计算）。

时间戳与句柄都在签名原料里，所以改期、换句柄都会对不上；
没有 Key 则算不出签名。容差与有效期由 `VIDLINK_SIG_TTL` 控制（默认 30 秒）。

两种拿到签名的方式，任选：

```bash
# A. 问服务端要（最省事）：GET /v1/sign 见 §3.6a
Q=$(curl -s -H "X-API-Key: $KEY" "$BASE/v1/sign" | sed -n 's/.*"query":"\([^"]*\)".*/\1/p')
curl "$BASE/v1/links?url=$URL&$Q"

# B. 本机自己签（少一次往返；三个示例脚本见下）
sh examples/sign.sh "$KEY" "" acc_ /v1/links
```

#### 现成示例代码（可直接跑，仓库里就有的六个文件）

| 文件 | 语言 / 依赖 | 作用 |
| --- | --- | --- |
| `examples/sign.py` | Python 3（仅标准库） | 生成签名凭据、拼 URL |
| `examples/sign.js` | Node 18+ / 浏览器（零依赖） | 同上（**页面内嵌的就是这套实现**） |
| `examples/sign.sh` | POSIX sh + openssl | 同上 |
| `examples/parse.py` | Python 3 | 完整解析客户端：`info`/`links`/`detail`、用量、代理下载 |
| `examples/parse.js` | Node 18+ | 同上 |
| `examples/parse.sh` | sh + curl | 同上（不需要 jq） |

```bash
export VIDLINK_BASE=https://vl.wzml.cc.cd
export VIDLINK_KEY=vl_xxx

# 解析（三种语言，输出一致）
python3 examples/parse.py --url 'https://www.bilibili.com/video/BV1xx411c7mD'
node    examples/parse.js --url 'https://www.bilibili.com/video/BV1xx411c7mD' --endpoint info
sh      examples/parse.sh --url 'https://www.bilibili.com/video/BV1xx411c7mD' --download out.mp4
python3 examples/parse.py --usage          # 看自己的配额

# 只要签名（三份实现输出逐字符相同，已由仓库测试保证）
python3 examples/sign.py "$VIDLINK_KEY" 1755571800
node    examples/sign.js "$VIDLINK_KEY" 1755571800
sh      examples/sign.sh "$VIDLINK_KEY" 1755571800
```

最小内联版（不想打开文件时抄这段）：

```python
# Python：签名 + 调用
import hashlib, hmac, time, urllib.request

def credential(key, ts=None):
    ts = ts or int(time.time())
    h = "acc_" + hashlib.sha256(key.encode()).hexdigest()[:16]
    th = "%08x" % ts
    sig = hmac.new(key.encode(), ("%s|%s" % (h, th)).encode(), hashlib.sha256).hexdigest()
    return "%s.%s.%s" % (h, th, sig)

req = urllib.request.Request(BASE + "/v1/links?url=" + urllib.parse.quote(url),
                             headers={"X-API-Key": credential(KEY)})
print(json.load(urllib.request.urlopen(req)))
```

```js
// 浏览器：页面已内嵌同一实现（VLSign），控制台里可直接用
const cred = VLSign.credential("acc_", key);
const r = await fetch(base + "/v1/links?url=" + encodeURIComponent(url),
                      { headers: { "X-API-Key": cred } });
console.log(await r.json());

// <a download> / <video> 这种加不了请求头的场景：签名进 URL
const href = VLSign.signUrl("acc_", key, "/v1/proxy?url=" + encodeURIComponent(direct));

// Node：同一份文件也能直接 require（examples/sign.js 同时导出 CommonJS）
const S = require("./examples/sign.js");
console.log(S.credential("acc_", key));
```

失败一律 `403`，并且**不区分**"Key 不存在"与"Key 错误"（避免被用来枚举
有效 Key）。签名相关的错误码另给，便于自查：

| `error.kind` | 含义 | 怎么办 |
| --- | --- | --- |
| `key_format` | `?key=` 不是签名形态（例如放了明文 Key） | 改用请求头，或用 `GET /v1/sign` 换签名 |
| `signature_expired` | 时间戳太旧 | 重新签发；脚本请每次现签（示例都是现签） |
| `signature_future` | 时间戳在未来 | 用 `/v1/health` 的 `time` 对本机时钟 |
| `signature_invalid` | 签名对不上 | Key 不对，或凭据被改过 |
| `key_scope` | 拿管理签名打了解析接口（或反之） | 账号用 `acc_`，管理用 `adm_` |

```json
{"error": {"kind": "key_format", "message": "URL 里的 key 只能是签名凭据 …", "request_id": "..."}}
```

Key 由管理 Key 创建（见 §3.9）。**Key 的明文只在创建时返回一次**，
之后所有接口只返回掩码；若丢失，请删除该账号并重建。
若创建时传入的 Key 没有 `vl_` 前缀，服务端会自动补上，并在响应里
用 `notice` 说明最终值（已有账号的 Key 不会被改动）。

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
- 批量：只按**成功条数**扣；搜索同样按**返回条数**扣（默认 0.25/条，
  搜不到就是 0）；
- 代理按**传输体积**扣（0.5 配额/MiB，不乘平台系数）；
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

> 下面按"公开 → 计量 → 账号 → 管理 → 运维"排列；所有路径都是**相对路径**，
> 示例统一用 `$BASE`（见文首定义）。

### 3.1 `GET /v1/version` · 公开

```json
{"version": "v0.88", "api_version": "v1", "platform": "linux/arm64"}
```

### 3.2 `GET /v1/health` · 公开

```json
{
  "status": "ok", "version": "v0.88", "api_version": "v1",
  "uptime_sec": 3821, "requests": 10422, "errors": 37,
  "time": 1755571820, "sign_ttl": 30,
  "cache": {"entries": 128, "hits": 9014, "misses": 1408, "coalesced": 52, "loads": 1408}
}
```

`time`（unix 秒）与 `sign_ttl`（秒）是给签名排障用的：报 `signature_expired`
或 `signature_future` 时，用本机 `date +%s` 与 `time` 一减就知道时钟偏了多少
（本机自己签名的场景下这是最常见的原因）。

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

### 3.6a `GET /v1/sign` · 需 Key · 不消耗配额 · 免校验模式下不存在

用当前 Key 换一条**签名凭据**，专门给"浏览器原生请求"用——
`<a download>`、`<video>`、混流器的 `fetch` 都加不了自定义请求头，
只能把凭据放进 URL，而 URL 里只接受签名。

```bash
curl -H "X-API-Key: $KEY" "$BASE/v1/sign"
```

```json
{
  "key": "acc_17090c89a1ca6ee3.68a3e658.35c1daf1f0dfefd2bb15906d25ff8e6e5dfaf2ce9757749b7ccbcb0c4a2a46c9",
  "query": "key=acc_17090c89a1ca6ee3.68a3e658.35c1da…",
  "type": "account",
  "ttl": 30,
  "issued_at": 1755571800,
  "expires_at": 1755571830,
  "note": "签名有效期 30 秒，且不绑定具体请求…"
}
```

用法：把 `query` 原样拼到任意需要 Key 的 URL 后面。

```bash
# 先换签名，再拼进 URL（<a>/<video> 场景）
Q=$(curl -s -H "X-API-Key: $KEY" "$BASE/v1/sign" | sed -n 's/.*"query":"\([^"]*\)".*/\1/p')
curl "$BASE/v1/usage?$Q"
```

三点须知：

- **不绑定具体请求**：拿到这条签名的人在有效期内可以调用该账号的**任意**
  接口，消耗记在该账号账上并出现在它的流水里。所以它只适合"马上要用"。
- **有效期就是容差**：`|服务器时间 − 时间戳| ≤ ttl`（默认 30 秒，
  `VIDLINK_SIG_TTL` 可调）。当服务端与验证端是同一台机器时不存在时钟偏差，
  这个容差主要是留给"签发后隔一会儿才用"和"本机自己签"的场景。
- **代理下载要留神续传**：浏览器对大文件的断点续传会用同一个 URL 再发一次
  请求，如果已经超过有效期就会 `403 signature_expired`。大文件场景请把
  `VIDLINK_SIG_TTL` 调大（如 300）。

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

### 3.8a 公共 Key（`vl_public`）· 免注册试用

账户模式下服务会自动准备一个**公共账号**（`VIDLINK_PUBLIC_KEY`，默认 `vl_public`）：

| 项 | 行为 |
| --- | --- |
| 额度 | **每 IP 每日 25 配额**（`VIDLINK_PUBLIC_DAILY_QUOTA`）；端点系数与普通账号相同，代理也是统一费率 **0.5 配额/MiB** → 约 50 MiB 代理流量 |
| 响应头 | `X-Quota-Consumed` 是本次消耗，`X-Quota-Remaining` 是**本 IP 今天**的剩余 |
| 超限 | `429 public_quota_exhausted`，message 提示申请独立 Key；额度按本地时区零点重置 |
| 没带 Key | `403` 的 message 里直接给出公共 Key 与每日额度 |
| `/v1/usage` | 返回 `public: true` + `daily: {daily_limit, used_today, remaining, resets_at}`；`quota` 字段是"本 IP 今日剩余"（兼容） |
| `/v1/ledger` | **403**（共享账号，流水混有所有访客）；普通账号不受影响，仍能读自己的账单 |
| 并发 | 按 IP 串行（不是按 Key） |
| 记账 | 用量与调用次数累计到公共账号，管理面可见；`/v1/admin/stats` 的 `public.ips_today` 是今天用过的 IP 数 |
| 关闭 | 管理面板停用公共账号，或把 `VIDLINK_PUBLIC_KEY` 留空 |

> 每 IP 的日计数**只在内存里**，重启清零——持久化要每次计费写一次 SD 卡，对这个量级不值得。

### 3.8a2 每日签到 `POST /v1/checkin` · 需 Key · 不消耗配额

账号上由管理员设置两个属性：`daily_grant`（每日签到可领配额）与
`grant_cap`（停止增加界限，0 = 不封顶）。用户自己调这个接口领取，**每天一次**：

```json
{
  "granted": 25, "balance": 125, "daily": 25, "cap": 200,
  "already_checked_in": false, "at_cap": false, "checked_in": true,
  "next_checkin_at": "2026-09-18T00:00:00+08:00", "unit": "配额",
  "message": "签到成功：+25 配额，当前剩余 125"
}
```

```bash
curl -X POST -H "X-API-Key: $KEY" $BASE/v1/checkin
```

| 情况 | 响应 |
| --- | --- |
| 签到成功 | `200`，`granted>0`、`checked_in=true`，流水里多一条 `add`：`每日签到 +25 → 125（上限 200）` |
| 今天已签 | `200`，`already_checked_in=true`、`granted=0`（**不是错误**，客户端据此显示"明天再来"） |
| 余额已达界限 | `200`，`at_cap=true`、`granted=0`，**不占用当天的签到机会**（花掉一些后当天仍可签） |
| 账号未开放签到（`daily_grant=0`） | `400` + 提示找管理员设置额度 |
| 公共 Key | `403`（它的额度是每 IP 每日自动给的，与账本余额无关） |
| 账号停用 | `403` |

**签到是主动的**：服务不会在每天第一次调用时自动补额，也不会因为解析或
读接口"顺手"加配额——加配额只发生在 `POST /v1/checkin`。

`GET /v1/usage` 里 `checkin` 段**始终存在**（哪怕不能签），字段：
`enabled`（**可否签到**，这是客户端该看的字段）、`daily`、`cap`、
`checked_in_today`、`last_checkin_day`、`endpoint`、`formula`。
用"字段不存在"表示不能签会让客户端只能猜，所以宁可多一个布尔。

管理面的账号视图带派生的 `can_check_in`（= `daily_grant > 0` 且未停用），
面板据此显示「可签到 / 未开放」。

### 3.8b 配额流水 `GET /v1/ledger` · 需 Key · 不消耗配额

返回**自己账号**的流水（新的在前）：使用、管理员增加/减少/设置、建号/删号。
接口里没有任何"读别人"的入口——服务端直接按当前 Key 限定。

```json
{
  "scope": "self",
  "account": {"id": "acc_1f2e…", "name": "示例账号", "key": "vl_a********3f7c"},
  "unit": "配额",
  "entries": [{
    "time": "2026-01-02T03:04:05Z", "type": "consume", "key": "vl_a********3f7c",
    "id": "acc_1f2e…", "name": "示例账号",
    "units": -1.2, "balance": 98.8, "used": 12.7, "calls": 14,
    "detail": "detail/bilibili ×1"
  }],
  "totals": {"consume": {"count": 9, "units": -12.7}, "add": {"count": 2, "units": 200}},
  "types": {"consume": "使用：解析或代理消耗配额（units 为负）", "...": "..."},
  "limit_max": 500,
  "note": "流水只保留最近若干条，汇总为全量口径；本接口不消耗配额"
}
```

| 参数 | 说明 |
| --- | --- |
| `limit` | 条数上限，默认 50、最大 500；非正整数 → `400` |
| `type` | `consume` / `add` / `reduce` / `set` / `create` / `delete`；不认识 → `400` |

字段含义：

| 字段 | 说明 |
| --- | --- |
| `units` | **带符号**的变化量：消耗为负，管理员增加为正 |
| `balance` | 这条流水之后的剩余配额 |
| `detail` | 人能读的说明：`links/bilibili ×1`、`proxy 66.53 MiB（按体积）`、`管理员增加 100 配额`、`账号倍率 1 → 0.5` |
| `totals` | 按类型汇总（**全量**口径，不受 `entries` 条数上限影响）；"累计消耗" = `-totals.consume.units` |

> 用账号**重置过 Key** 时，此前的流水会并入新 Key 名下（账号记着历史 Key），
> 所以"这个账号一共花了多少"不会因为换钥匙而少一截。
>
> 流水与账号快照共用一个账本文件、共用一次 `fsync`，重启一起回放，所以历史不会丢。
> 接口只承诺"最近 N 条 + 全量汇总"，更早的逐条记录可以直接读账本文件。

### 3.8c 搜索 `GET /v1/search` · 需 Key · 0.25/条 · 免校验模式下不计量

按关键词搜内容。返回的是**元信息 + id**，**不含任何直链**——
搜索是入口，取直链仍走 `links` / `detail`（这正是它能按条计价的前提）。

```
GET /v1/search?platform=bilibili&keyword=猫&limit=3
```

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `platform` | ✅ | `bilibili` \| `jianpian`；其它平台 `400` 并说明原因（见下） |
| `keyword` | ✅ | 关键词，1~64 字符（别名 `q`） |
| `page` | ❌ | 页码，默认 1，1~50 |
| `limit` | ❌ | 每页条数，默认 20，1~50（**按实际返回条数计价**） |

```json
{
  "platform": "jianpian", "keyword": "太空", "page": 1, "limit": 20,
  "count": 20, "total": 2940, "has_more": true, "next": "page=2&limit=20",
  "cost": 5,
  "items": [{
    "platform": "jianpian", "type": "series", "id": "553300",
    "title": "太空部队", "desc": "…", "cover": "https://img…/a.jpg",
    "score": 8, "year": 2020, "category": "电视剧", "latest": "第10集",
    "actors": ["史蒂夫·卡瑞尔"],
    "detail": "GET /v1/detail?platform=jianpian&id=553300"
  }]
}
```

| 字段 | 说明 |
| --- | --- |
| `cost` | 本次实际扣减（= 0.25 × `count` × 账号倍率）；空结果不出现该字段 |
| `count` / `total` | 本次条数 / 平台声明的匹配总数（平台可能截断，如 B 站上限 1000） |
| `has_more` / `next` | 是否还有下一页，以及下一页的现成参数 |
| `items[].id` | **拿去 detail/links 的那个 ID**（B 站 bvid；荐片影片 ID） |
| `items[].detail` | 现成的下一步调用提示（不用自己拼 URL） |

平台支持（2026-09 实测，过难放弃的都写明原因，`/v1/platforms` 里同样给出）：

| 平台 | 支持 | 原因 |
| --- | --- | --- |
| `bilibili` | ✅ | B 站 `search/type` 匿名可用；风控时自动换 WBI 签名通道重试 |
| `jianpian` | ✅ | 荐片 `search/videoV2`（10 条/页，取 20/50 条由服务端拼页） |
| `douyin` | ❌ | 需要登录态（未登录实测 `status_code=2483`） |
| `kuaishou` | ❌ | 反爬校验（实测 `result=400002` + 滑块验证） |
| `xiaohongshu` | ❌ | 需要 `x-s`/`x-s-common`/`x-rap-param` 全套签名与登录态 |

> 缓存：同一 `(平台, 关键词, 页, 条数)` 10 分钟内命中服务端缓存（省上游），
> 但**缓存命中照样按条计费**——缓存省的是上游请求，不是客户的额度。

### 3.9 管理端点 `/v1/admin/*` · 需固定管理 Key · 免校验模式下不存在

> `VL_EASE=true` 时管理面整体不注册，所有 `/v1/admin/*` 返回 `404`。

**管理凭据是一个固定的 Key**，来自环境变量或 `.vl` 里的 `VIDLINK_ADMIN_KEY`；
它不是账本里的账号，账号也没有任何权限位：

| 情况 | 结果 |
| --- | --- |
| 请求里的 Key == `VIDLINK_ADMIN_KEY` | 放行 |
| 是账本里的账号 Key（哪怕配额很高） | `403 forbidden` |
| 没有配置 `VIDLINK_ADMIN_KEY` | `403 forbidden`，message 里点名该配置项 |
| 传 Key 的方式 | 明文走 `X-API-Key` / `Authorization: Bearer`；URL 里只收**管理签名** `?key=adm_…`（`GET /v1/admin/sign` 换取） |

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
{"name": "新账号", "quota": 100, "multiplier": 1.0,
 "daily_grant": 25, "grant_cap": 200, "note": "备注"}
```

请求体里出现未知字段（例如老接口的 `admin`）会返回 `400`，而不是被静默忽略。

- 不填 `key` 时自动生成 256 位随机 Key；
- 填了 `key` 但**没有 `vl_` 前缀**时，服务端会自动补上，并在响应里用
  `notice` 说明最终值（`key` 字段给的就是补好之后的那个）。已有账号的 Key
  不会被改动——改 Key 等于把正在用的凭据作废；
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
| 开放每日签到（每天可领 25，余额上限 200） | `{"daily_grant": 25, "grant_cap": 200}` |
| 关闭每日签到 | `{"daily_grant": 0}` |

#### `DELETE /v1/admin/accounts/{key}`

删除账号（写删除墓碑，重启不会复活）。路径参数同样可以用明文 Key 或句柄 `id`。

删掉任何账号（包括唯一那个）都不会影响管理面本身——管理 Key 不在账本里。

#### `POST /v1/admin/accounts/{key}/reset_key` · 重置指定账号的 Key

把某个账号的 Key 换成新值：**换锁不换房子**——配额、用量、倍率、
签到状态、备注、创建时间全部保留，旧 Key 立刻失效。

```
POST /v1/admin/accounts/acc_1f2e…/reset_key
{"key": "vl_客户自定义的key"}     # 省略整个 body（或 {"key": ""}）= 服务端随机生成
```

```json
{
  "account": {"id": "acc_9a8b…", "key": "vl_n********7d21", "name": "客户甲", "quota": 7.5},
  "key": "vl_n5f3…（明文，只在这一次响应里出现）",
  "handle": "acc_9a8b…",
  "old_handle": "acc_1f2e…",
  "notice": "旧 Key 已立刻失效；请立即保存新 Key，它只会出现这一次。配额、用量与此前的历史账单都并入新账号（按新 Key / 新句柄即可查到全部流水）"
}
```

| 规则 | 说明 |
| --- | --- |
| 手填还是随机 | 不传 `key` → 生成 256 位随机 Key（推荐，避免弱 Key）；手填会自动补 `vl_` 前缀并如实回报 |
| 旧的立刻失效 | 在账本的同一把锁内完成切换，不存在"两把钥匙都能开"的窗口 |
| 明文只回一次 | 与建号一致；此后所有接口只回掩码。Key 是长期凭据，进了日志/历史/截屏就等于泄漏 |
| 句柄会变 | 句柄由 Key 派生（`acc_` + Key 的 SHA-256 截断），所以重置后句柄也变，响应里给出新旧句柄 |
| 查重 | 明文 Key 与它派生的**句柄（acc）**都查重：句柄是外键（面板/流水/日志都按它寻址），撞了会互相覆盖 → `400` |
| 历史流水 | **账单跟着账号走**：重置时会把旧 Key 记进账号的 `key_history`，此后按**新 Key / 新句柄**查流水与汇总，都能看到重置之前的全部记录 |
| 公共账号 | `400`：它的 Key 来自配置项 `VIDLINK_PUBLIC_KEY`，改账本副本只会造成"面板显示一个值、实际生效另一个值" |
| 冲突 | 目标 Key 已被占用（含"新旧相同"）→ `400` |

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

#### `GET /v1/admin/ledger` · 不消耗配额

配额流水：**不带条件就是总账单**（全部账号的汇总 + 最近流水），
带 `account=`（句柄 `acc_…` 或明文 Key，`id=` 是同义的旧参数名）则只读那个账号。

> 参数名不是 `key=`：那个名字在这里是"凭据"的意思，而 URL 里只收签名；
> 用 `?key=<明文>` 会被当成一次认证尝试并因格式错误 `403`。

```bash
curl -H "X-API-Key: $ADMIN" '$BASE/v1/admin/ledger?limit=100'
curl -H "X-API-Key: $ADMIN" '$BASE/v1/admin/ledger?account=acc_1f2e3d4c5b6a7980&type=set'
```

```json
{
  "scope": "account",
  "account": {"id": "acc_1f2e…", "name": "示例账号", "key": "vl_a********3f7c",
              "quota": 98.8, "used": 12.7, "calls": 14, "multiplier": 1, "disabled": false},
  "entries": [{"time": "…", "type": "add", "name": "示例账号", "units": 100,
               "balance": 198.8, "detail": "管理员增加 100 配额"}],
  "totals": {"add": {"count": 1, "units": 100}},
  "unit": "配额", "types": {"…": "…"}, "limit_max": 500,
  "note": "流水只保留最近若干条，汇总为全量口径；本接口不消耗配额。不带 account/id 即总账单"
}
```

- `scope` 为 `all`（总账单）或 `account`；不带 `account`/`id` 时是前者；
- 参数与用户接口一致（`limit` / `type`），另加 `account`（句柄 `acc_…` 或明文 Key；`id` 同义）；
- 账号不存在 → `404`；不是管理 Key → `403`；免校验模式下整条路由不存在 → `404`。

#### `GET /v1/admin/sign` · 不消耗配额

用管理 Key 换一条**管理签名**（`adm_…`），给"URL 里需要凭据"的场景用：
管理面板的接口直连链接就是这样生成的（页面本地算签名，链接约 30 秒有效）。

```bash
curl -H "X-API-Key: $ADMIN" "$BASE/v1/admin/sign"
```

```json
{
  "key": "adm_9c1f4e2ab7d35f60.68a3e658.2f7b…（64 位签名）",
  "query": "key=adm_9c1f4e2ab7d35f60.68a3e658.2f7b…",
  "type": "admin", "ttl": 30,
  "issued_at": 1755571800, "expires_at": 1755571830
}
```

```bash
# 拿一条管理签名去打只读管理接口（URL 里没有明文管理 Key）
Q=$(curl -s -H "X-API-Key: $ADMIN" "$BASE/v1/admin/sign" | sed -n 's/.*"query":"\([^"]*\)".*/\1/p')
curl "$BASE/v1/admin/stats?$Q"
```

- 管理签名只能在 `/v1/admin/*` 上用；账号签名（`acc_`）打管理接口会 `403 key_scope`，反之亦然。
- 句柄必须是由 `VIDLINK_ADMIN_KEY` 派生出来的那一个，否则 `403 key_invalid`（换了管理 Key，旧签名立刻失效）。

#### `GET /v1/admin/quota` · 不消耗配额

计费倍率全貌：当前**生效值**、覆盖层（改过的格子）、出厂默认、
预授权上限、校验范围、定价告警与最近修改历史。

```json
{
  "unit": "配额",
  "formula": "消耗 = 端点系数(端点, 平台) × 条数 × 账号倍率",
  "endpoints": {"info": "仅元信息…", "links": "仅直链…", "detail": "…", "batch_links": "…"},
  "platforms": {"bilibili": {"info":0.5,"links":1.0,"detail":1.2,"batch_links":0.75},
                "douyin":   {"info":0.75,"links":1.1,"detail":1.5,
                             "batch_links":{"unsupported":true,"reason":"抖音按 IP 维度限流…"}}},
  "default_platform": {"info":0.5,"links":1.0,"detail":1.2,"batch_links":0.75},
  "preauth_max": {"info":0.75,"links":1.1,"detail":1.5,"batch_links":0.75},
  "overrides": {"links": {"douyin": 1.5}},
  "defaults":  {"links": {"default": 1.0, "douyin": 1.1}},
  "source": "file", "path": "data/rates.json", "updated_at": "2026-09-18T22:10:00+08:00",
  "limits": {"min_rate": 0, "max_rate": 100},
  "warnings": ["detail(1.2) ≤ info+links(1.5)：打包折扣消失"],
  "history": [{"at": "2026-09-18T22:10:00+08:00", "action": "update", "actor": "admin",
               "detail": "douyin/links: 1.1 → 1.5"}],
  "proxy": {"rate": "0.5 配额/MiB", "rate_per_mib": 0.5, "unit_bytes": 1048576,
            "platform_factor": false, "formula": "…", "note": "…"},
  "public": {"enabled": true, "key": "vl_public", "daily_per_ip": 25, "...": "..."},
  "note": "系数只影响后续调用，已发生的用量不重算；单个账号的调整请改该账号的 multiplier"
}
```

| 字段 | 说明 |
| --- | --- |
| `platforms` | **当前生效值**（覆盖 → 通用档 → 出厂默认逐层回退后的结果） |
| `default_platform` | 通用档（未单列的平台走它） |
| `overrides` / `defaults` | 改过的格子 / 出厂默认值，面板据此标出"哪一格被改过" |
| `source` | `defaults`（无覆盖）或 `file`（从倍率文件加载） |
| `preauth_max` | 各端点的预授权上限，**改价后立刻重算** |
| `warnings` | 定价提示（如"detail 比 links 还便宜"），**只提示不阻止** |
| `history` | 最近 50 条变更（`action` = `update` / `reset`；`actor` 恒为 `admin`，因为管理面只有一个固定 Key，没有多管理员身份） |

#### `PUT /v1/admin/quota` · 改计费倍率 · 不消耗配额

**逐格合并**：只提交要动的格子，其余保持不变；值为 `null` 表示**清除该格**、
回到出厂默认。任何一格非法 → 整请求 `400`，不会部分生效。

```bash
# 抖音 links 改成 1.5；快手的 info 恢复出厂默认；代理费率改成 0.6
curl -X PUT -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"rates":{"douyin":{"links":1.5},"kuaishou":{"info":null}},"proxy_rate":0.6}' \
  "$BASE/v1/admin/quota"
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `rates` | 否 | `平台 → 端点 → 倍率\|null`。平台取值：`bilibili` / `douyin` / `kuaishou` / `xiaohongshu` / `default`（通用档）；端点：`info` / `links` / `detail` / `batch_links` |
| `proxy_rate` | 否 | 代理费率（配额/MiB，`0` 表示代理不扣配额，所有账号同价、**不乘平台系数**） |

- 范围 `0 ~ 100`（`max_rate` 是防手滑，不是运营判断）；
- **抖音 × `batch_links` 不能设**（平台能力问题，不是价格）→ `400`；
- 两项都省缺 → `400`（没有要改的东西）；
- 成功 `200`，响应与 `GET` 同形（面板一次往返即可重渲染）；
- 改动**只影响后续请求**：已经预授权的请求按旧价结算；
- 落盘失败（磁盘满/只读）→ `500`，内存已回滚；倍率文件损坏会**拒绝启动**
  （宁可起不来，也不带着半个价目表跑）；
- 存储：`data/rates.json`（`VIDLINK_RATES_PATH`，原子写、权限 600），只存
  "改过的格子"，出厂默认仍在代码里，所以重置只是丢掉覆盖层。

#### `DELETE /v1/admin/quota` · 恢复出厂默认 · 不消耗配额

清空全部覆盖（含代理费率），返回与 `GET` 同形的结果。管理面板上的
「恢复内置默认」按钮就是它。想只改一个值请用上面的 `PUT`。

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
实扣 = 传输体积(MiB) × 0.5 × 账号倍率        # 统一费率，不乘平台系数
```

- 上游声明了长度（`Content-Length`；Range 请求时它就是这一段的长度）→ **先扣后传**：
  配额不足直接 `429 quota_exhausted`，一个字节都不发，`X-Quota-*` 如实回写；
- 长度未知（chunked）→ 按账户余额折算字节上限，边传边限，传完按实际字节补扣
  （这条路径来不及回写 `X-Quota-*`）；
- `HEAD` 不产生响应体，**不计费**；
- 提前中断**不退**（按声明长度计费是"这次占用了多少出口带宽"的度量）；
- 计费精度 0.0001 配额（约 105 字节）；
- **所有账号同一费率**（默认 0.5 配额/MiB，可用 `PUT /v1/admin/quota` 的
  `proxy_rate` 或首次部署时的 `VIDLINK_PROXY_RATE` 调整）；
  **永不参与平台系数**——把平台系数改到天上也不会影响代理计费；
  1 GB ≈ 512 配额，70 MB 的视频 ≈ 35 配额；
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
- **画面内的强制水印无法去除**（平台侧烧进画面，业界普遍如此）；
- **番剧（PGC）支持 ep 链接**：`/bangumi/play/ep733316` 可直接解析（也支持
  `platform=bilibili&id=ep733316` 与 BV / av 链接）。实现是"入口走 PGC、
  取流复用普通通道"：先按 `ep_id` 查季信息拿到该集的 `bvid/cid`，
  再走原来的 `view → playurl`。实测匿名最高 **1080P**，且是整集（不是试看）；
  会员/付费/未放送的集数会返回 `403` 并说明原因。
  **季链接（`ss…`）会被拒绝**：一部番几百集，猜"第 1 集"是错的——
  请用具体一集的地址。

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
  "$BASE/v1/links?url=https://www.bilibili.com/video/BV1xx411c7mD&quality=1080")

v=$(echo "$j" | python3 -c 'import sys,json;print(json.load(sys.stdin)["url"])')
a=$(echo "$j" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("audio_url",""))')

curl -H "Referer: https://www.bilibili.com/" -H "User-Agent: Mozilla/5.0" -o v.m4s "$v"
curl -H "Referer: https://www.bilibili.com/" -H "User-Agent: Mozilla/5.0" -o a.m4s "$a"
ffmpeg -i v.m4s -i a.m4s -c copy out.mp4
```

### 6.2 挑一条"树莓派能播"的流

```bash
curl -s -H "X-API-Key: $KEY" "$BASE/v1/detail?url=$URL" |
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
  $BASE/v1/batch/links |
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
  "$BASE/v1/links?url=$URL" | grep -i x-quota
# X-Quota-Consumed: 1
# X-Quota-Remaining: 99

curl -s -H "X-API-Key: $KEY" $BASE/v1/usage
```

### 6.5 管理：补配额、给活动账号减半

```bash
# 补 100 配额
curl -s -X PATCH -H "X-API-Key: $ADMIN" -H "Content-Type: application/json" \
  -d '{"add_quota":100}' $BASE/v1/admin/accounts/vl_xxx

# 该账号按 0.5 倍消耗（活动 / 内部）
curl -s -X PATCH -H "X-API-Key: $ADMIN" -H "Content-Type: application/json" \
  -d '{"multiplier":0.5}' $BASE/v1/admin/accounts/vl_xxx
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
| GET | `/tip.png` | 公开 | —（赞赏码静态图） |
| GET | `/v1/platforms` | 公开 | — |
| GET | `/healthz` | 公开 | — |
| GET | `/readyz` | 公开 | — |
| GET | `/v1/usage` | Key | — |
| GET | `/v1/ledger` | Key | —（自己的流水） |
| GET | `/v1/sign` | Key | —（换一条 30 秒签名，给 URL 用） |
| POST | `/v1/checkin` | Key | —（每日签到领配额） |
| GET | `/v1/search?platform=&keyword=` | Key | 0.25/条（默认 20 条，可 `page`/`limit`） |
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
| POST | `/v1/admin/accounts/{key\|id}/reset_key` | 管理 Key | —（换 Key，配额与用量保留） |
| GET | `/v1/admin/stats` | 管理 Key | — |
| GET | `/v1/admin/quota` | 管理 Key | —（计费倍率全貌） |
| PUT | `/v1/admin/quota` | 管理 Key | —（改倍率：逐格合并，null 恢复默认） |
| DELETE | `/v1/admin/quota` | 管理 Key | —（恢复出厂默认） |
| GET | `/v1/admin/ledger` | 管理 Key | —（总账单 / `?account=` 指定账号） |
| GET | `/v1/admin/sign` | 管理 Key | —（换一条 30 秒管理签名） |

其他文档：

- 机器可读规格：[`openapi.yaml`](openapi.yaml)
- 配额倍率说明：[`配额倍率表.md`](配额倍率表.md)
- 架构与取舍：[`架构设计.md`](架构设计.md)
- 平台调研（实测数据）：[`平台调研报告.md`](平台调研报告.md)
