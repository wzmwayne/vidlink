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

完整接口文档（全部端点、示例、错误码）见 **[docs/API.md](docs/API.md)**；
这里只跑通三步。官方测试实例是 <https://vl.wzml.cc.cd>（公共 Key `vl_public` 免注册可试）。

```bash
# 构建
go build -o vidlink .

# 运行（默认监听 :8080，账本写到 ./data/accounts.jsonl）
# VIDLINK_ADMIN_KEY 决定管理接口是否可用：不配置就没人能改账号（解析照常）
VIDLINK_ADMIN_KEY=$(openssl rand -hex 32) ./vidlink

export BASE=http://127.0.0.1:8080        # 官方测试实例则是 https://vl.wzml.cc.cd
export ADMIN=$VIDLINK_ADMIN_KEY

# ① 建一个自己的账号（明文 Key 只在这次响应里出现一次）
curl -s -X POST -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"我自己","quota":1000}' "$BASE/v1/admin/accounts"

# ② 用它的 Key 取一条能播的直链（1.0 配额；只想试试就直接用公共 Key vl_public）
export KEY=vl_xxx

# 推荐：现算一张签名放进请求头（明文 Key 不进网络；示例脚本见 examples/）
curl -H "X-API-Key: $(sh examples/sign.sh "$KEY")" "$BASE/v1/links?url=https://www.bilibili.com/video/BV1GJ411x7h7"

# ③ 查自己的配额与用量
curl -H "X-API-Key: $(sh examples/sign.sh "$KEY")" "$BASE/v1/usage"
```

想用鼠标点：加 `VL_WEBUI=true` 启动，打开 `$BASE/`（解析页）与 `$BASE/admin`（管理面板）。
批量、字幕/图集、浏览器内混流、代理下载、每日签到、配额流水、改计费倍率等
都在 [docs/API.md](docs/API.md) 里。

### 客户端示例（Python / JS / Shell，零依赖）

`examples/` 里有六个可直接运行的文件，**都有测试在跑**（签名与 Go 侧逐字符比对、
解析示例会真的打一个会验签的桩服务端），所以它们不会腐烂：

```bash
export VIDLINK_BASE=https://vl.wzml.cc.cd
export VIDLINK_KEY=vl_xxx

python3 examples/parse.py --url 'https://www.bilibili.com/video/BV1xx411c7mD'   # 直链
node    examples/parse.js --url '...' --endpoint info                          # 元信息
sh      examples/parse.sh --url '...' --download out.mp4                       # 代理下载
python3 examples/parse.py --usage                                              # 我的配额

python3 examples/sign.py "$VIDLINK_KEY"    # 只要签名（三份实现输出一致）
node    examples/sign.js "$VIDLINK_KEY"
sh      examples/sign.sh "$VIDLINK_KEY"
```

**为什么推荐签名而不是明文 Key**：明文 Key 是长期凭据，它一进 URL 就会留在
浏览器历史、下载记录、隧道与反向代理日志、聊天记录与截屏里，泄漏一次就等于
把账号交出去；签名只有 30 秒有效期，且签名本身不含秘密。vidlink 的两个内嵌
页面（解析页、管理面板）就是这么发的——明文 Key 只存在本机浏览器里，
**不进任何一次请求**。规则与算法见 [docs/API.md](docs/API.md) §2.2。

## 图形化页面（VL_WEBUI）

根路径 `/` 可以返回一个自包含的解析页：填链接、选清晰度、点按钮，
结果按直链 / 请求头 / 视频轨 / 音频轨 / 字幕 / 图集 / 原始 JSON 分区渲染，
带批量解析、浏览器内混流、代理下载，全部零外部资源。

```bash
VL_WEBUI=true ./vidlink      # 账户模式下打开（免校验模式默认就是开的）
VL_WEBUI=false ./vidlink     # 任何模式下都能关掉
```

**两种模式下的差别**：

| | 免校验模式 | 账户模式 |
|---|---|---|
| 页面本身 | 公开 | **公开**（否则用户没地方填 Key） |
| 数据请求 | 无需凭据 | **页面里必须填 API Key** |
| Key 存哪 | — | 只存在本机浏览器 `localStorage` |
| 请求带什么 | — | **现算的签名**（明文 Key 不出本机） |
| 页面内容 | 顶部提示"不校验、不计量" | 多出「API Key」与「账户与用量」两块 |

账户模式下的用法：打开页面 → 在「API Key」里填入 Key → 保存。
填好后会自动拉一次 `GET /v1/usage`，展示**账号名、剩余配额、累计消耗、
累计调用次数、账号倍率、并发与批量限制、各平台各端点系数**；
Key 无效或过期时会明确提示 403 并高亮输入框。

> 页面本体不含任何数据与凭据，数据全靠页面里的 Key 去请求。
> 账户模式下代理下载、混流取流走的是**签名**链接
> （`?key=acc_<句柄>.<时间戳>.<签名>`，页面用纯 JS 现算，约 30 秒有效）——
> `<a>` / `<video>` 无法自定义请求头，但 URL 里也**绝不放明文 Key**：
> 明文 Key 会留在浏览器历史、下载记录与代理日志里，而它长期有效。

## 免校验模式（VL_EASE=true）

想把它当**纯解析工具**用——不建账号、不发 Key、不算配额——加一个环境变量：

```bash
VL_EASE=true ./vidlink
# 之后所有解析端点都不需要任何凭据，例如 GET /v1/links?url=…
```

**浏览器打开 `$BASE/` 就是一个图形化解析页**：填链接点按钮即可，
结果按"直链 / 请求头 / 视频轨 / 音频轨 / 字幕 / 图集 / 原始 JSON"分区渲染，
带复制直链、复制 curl / ffmpeg 命令、批量解析（5~20 条）、平台能力表。

### 下载命名

页面里所有下载（含经服务端代理的下载）都用同一套命名，不再出现
"下载到一个叫 `proxy`、没有后缀的文件"：

```
<标题>VL<类型><清晰度/音频码率>.<后缀>

歌曲MV《我不曾忘记》VL前端混合1080P.mp4     ← 页面内混合后的产物
某视频VL原生混合720P.mp4                     ← 平台给的就是音视频一体
某视频VL视频1080P.mp4                        ← 分离流的视频轨
歌曲MV《我不曾忘记》VL音频192K.m4a            ← 分离流的音频轨
```

实现上由前端算好名字、经 `/v1/proxy?filename=...&type=...` 落到
`Content-Disposition`（中文按 RFC 2231 编码）；服务端会对文件名做消毒
（去控制字符/引号/路径分隔符、限长），因为它是查询参数且要写进响应头。

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

HLS(TS) 线路（荐片这类 m3u8）走另一条路：分片**直连 CDN** 下载（需要时用 AES-128
现场解密）→ 顺序拼接成单个 `.ts`，分片并发默认 **8** 路、界面可调（1~32）。
现代浏览器一般能直接预览 TS；合并完还可以点「**转成 MP4（不重编码）**」换盒子：

```
.ts（MPEG-TS）─→ 自写解复用（PAT/PMT/PES、Annex-B→AVCC、ADTS→裸 AAC、SPS 解宽高）
              ─→ 自写 MP4 索引（stts/ctts/stsz/co64/stss + avcC/esds）
              ─→ .mp4（moov 在文件尾）
```

- **只搬字节**：不解码、不重编码，画质音质与体积一个字节都不变，只是多一张索引表；
  产物对剪辑软件/相册/微信的接受度比 `.ts` 好得多；
- **边界说清楚**：只支持 H.264 + AAC；HEVC、AC-3、MP2 线路会**明确拒绝并保留 `.ts`**
  （封得进去也播不了，不如不装）；
- **测试方式**：ffmpeg 生成真实 TS（含 B 帧，才走得到 `ctts`）→ 页面里那段纯逻辑转封装
  → 再让 ffmpeg 解码源 TS 与产物，比较**解码后的字节哈希**，必须逐字节一致。

**两条实测得来的兜底逻辑**（都写在页面里）：

1. **CDN 节点差异**：多数节点带 `Access-Control-Allow-Origin: *`，浏览器可直连；
   但部分 B 站 P2P 节点（如 `*.edge.mountaintoys.cn`）**同时要求 Referer 与 UA**，
   缺任一个都 403，而这两个头浏览器禁止脚本设置。
   页面因此是三级兜底：直连 → 重新取链换节点 → 走 `/v1/proxy`
   （服务端补 Referer/UA；代理在免校验模式下**默认开启**，账户模式需显式
   `VIDLINK_PROXY_ENDPOINT=true`）。
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
| 管理接口 `/v1/admin/*` | 存在；凭据是 `VIDLINK_ADMIN_KEY` 这个**固定 Key**（没配就恒 403） | **404**（根本不注册） |
| 管理面板 `/admin` + 解析页 `/` | 视 `VL_WEBUI`（默认关） | 视 `VL_WEBUI`（默认开） |
| 配额计量与 `X-Quota-*` | 有 | **无**（不回写这两个头） |
| 按 Key 串行闸门 | 1 个并发解析 | 不适用（没有 Key） |
| 按 IP 限流 | `VIDLINK_RATE_LIMIT_RPM` | **关闭**（连同它一起关） |
| `/v1/usage`、`/v1/admin/*` | 存在 | **404**（根本不注册） |
| `/v1/proxy` | 默认关闭 | **默认开启**（显式 `false` 可关），且不校验白名单 |
| 根路径 `/` | 视 `VL_WEBUI`（默认纯文本导航页） | 视 `VL_WEBUI`（默认图形化页面） |
| `/v1/info`、`/v1/links`、`/v1/detail`、`/v1/batch/links` | 需 Key | **完全可用** |
| `/v1/platforms`、`/v1/version`、`/v1/health`、探针 | 公开 | 公开（不再返回 `rates`/`unit`） |
| 全局解析槽位（10 + 队列 30） | 有 | **保留**（这是资源保护，不是校验） |

两点刻意的设计：

- **保留全局解析槽位与按主机令牌桶。** 它们保护的是上游平台和这台机器的内存
  （1 GB 树莓派上跑 100 路并发解析会直接 OOM），与身份校验无关；
  想要更放开就调 `VIDLINK_GLOBAL_CONCURRENCY`。
- **免校验模式下 `/v1/usage` 与 `/v1/admin/*` 返回 404 而不是 403。**
  它们不被注册，外界探测不到"这里本该有个管理接口"；账户模式下它们存在，
  没有管理 Key 时给 403 并说明原因（装作不存在只会让人怀疑路径写错了）。

> ⚠️ **这个模式没有任何身份校验，绝不能暴露到公网。**
> 只用于本机、内网、或你完全控制的调用方。启动日志里会有一条 WARN 提醒。

## API

完整规格见 **[docs/API.md](docs/API.md)**（人读）与 **[docs/openapi.yaml](docs/openapi.yaml)**（机器读）。

| 方法 | 路径 | 鉴权 | 配额 | 说明 |
|---|---|---|---|---|
| GET | `/v1/version` | 公开 | — | 服务与接口版本 |
| GET | `/v1/health` | 公开 | — | 健康检查 + 缓存统计（含公共入口信息） |
| GET | `/tip.png` | 公开 | — | 解析页底部的赞赏码（内嵌静态图，`VL_WEBUI` 开启时才有） |
| GET | `/v1/platforms` | 公开 | — | 平台清单与各端点系数 |
| GET | `/healthz` | 公开 | — | **存活**探针（进程活着即 200） |
| GET | `/readyz` | 公开 | — | **就绪**探针（无可用平台时 503） |
| GET | `/v1/usage` | Key | — | 自己的配额、用量、倍率（免校验模式下不存在） |
| GET | `/v1/ledger` | Key | — | **自己的配额流水**：使用 / 管理员增加 / 减少 / 设置（免校验模式下不存在） |
| POST | `/v1/checkin` | Key | — | **每日签到领配额**（每天一次，`daily_grant`/`grant_cap` 由管理员设置） |
| GET | `/v1/usage` | Key | — | 自己的用量；`checkin.enabled` 明确告知**可否签到** |
| GET | `/v1/search?platform=&keyword=` | Key | **0.25/条** | 按关键词搜索（默认 20 条、可翻页），只给元信息 + ID，直链再走 links/detail |
| GET | `/v1/info?url=` | Key | 0.5 / 抖音 0.75 | 元信息 + 档位列表，**无直链**；B 站番剧用 `ep` 链接（季 `ss` 链接会提示改用 ep） |
| GET | `/v1/links?url=&quality=` | Key | 1.0 / 抖音 1.1 | **只有直链** |
| GET | `/v1/detail?url=` | Key | 1.2 / 抖音 1.5 | 元信息 + 全部档位直链 |
| POST | `/v1/batch/links` | Key | 0.75/条 | 批量直链，5~20 条，**无抖音** |
| GET | `/v1/proxy?url=` | Key | **0.5/MiB × 账号倍率** | 流式媒体代理（ease 模式默认开启，账户模式默认关闭） |
| GET | `/admin` | 公开（页面壳） | — | **管理面板**：填管理 Key 后管理账号（`VL_WEBUI` 开启且非 ease 模式） |
| GET/POST | `/v1/admin/accounts` | 管理 Key | — | 账号列表 / 创建账号（免校验模式下不存在） |
| GET/PATCH/DELETE | `/v1/admin/accounts/{key\|id}` | 管理 Key | — | 查 / 改 / 删账号（可用明文 Key 或账号句柄 `acc_…`） |
| POST | `/v1/admin/accounts/{key\|id}/reset_key` | 管理 Key | — | **重置账号 Key**（手填或随机；配额/用量保留、历史账单并入新账号，旧 Key 立刻失效） |
| GET | `/v1/admin/stats` | 管理 Key | — | 运行统计 |
| GET | `/v1/admin/quota` | 管理 Key | — | 配额系数全貌（只读） |
| GET | `/v1/admin/ledger` | 管理 Key | — | 配额流水：不带条件=总账单，`?account=`（或 `?id=`）指定账号 |
| GET | `/v1/sign` | Key | — | 换一条 30 秒**签名凭据**，给 URL（`<a>`/`<video>`）用 |
| GET | `/v1/admin/sign` | 管理 Key | — | 换一条 30 秒管理签名，给管理接口链接用 |

每个响应都带 `X-Request-Id`，错误体里也有同一个值，报障时直接提供即可定位：

```
$ curl -i -H 'X-API-Key: ...' '$BASE/v1/nope'
HTTP/1.1 404 Not Found
X-Request-Id: 91c0651a168bd9acaf6f85b2225d0c9f
{"error":{"kind":"not_found","message":"未知路径: /v1/nope","request_id":"91c0651a168bd9acaf6f85b2225d0c9f"}}
```

### 配额

```
一次调用的消耗 = 端点系数(端点, 平台) × 条数 × 账号倍率
```

- **只在实际成功时扣**：上游超时、内容不存在、限流、参数错误都不扣；批量只按成功条数扣；
- **媒体代理例外**：它按**传输体积**计费——`实扣 = 体积(MiB) × 0.5 × 账号倍率`，
  **不乘平台系数**（代理搬的是任意 CDN 的字节，与"哪家平台解析更贵"无关），
  **所有账号同一费率**（分档定价只会让"这次要花多少"变成查表的题）。
  0.5 这个数的依据：1 GB ≈ 512 配额、70 MB 视频 ≈ 35 配额；
  代理的真实成本是服务端出口带宽（家里上行 + 隧道），而解析一次只花上游几十 KB，
  1/MiB 会让"下一条片子"贵过"解析一百次"。
  上游给了 `Content-Length` 就先扣后传（不够直接 `429`，一个字节都不发）；
  长度未知（chunked）才边传边限、传完按实际字节扣。提前中断不退。
  计费精度到万分之一配额（约 105 字节）；
- 响应头回写 `X-Quota-Consumed` / `X-Quota-Remaining`；
- 解析前会按该端点的**最贵档位**做一次上限检查（预授权），不够直接 `429`，真正扣减仍按实际平台结算；
- 账号倍率由管理员设置：`0` 不扣配额（仍记调用次数）、`0.5` 减半、`1.0` 标准、`2.0` 加倍。

### 计费倍率（运行期可编辑）

倍率是**数据**，不是编译进代码的常量：4 个平台 × 4 个端点 + 通用档 + 代理费率，
都可以在运行期改，改完立刻对后续请求生效（预授权上限同步重算）。

| 事项 | 说明 |
| --- | --- |
| 出厂默认 | 代码里的价目表（`internal/quota`），永远可复现、可一键重置 |
| 改动存哪 | `data/rates.json`（`VIDLINK_RATES_PATH`）：**只存改过的格子**，原子写、权限 600 |
| 怎么改 | 管理面板的「计费倍率（可编辑）」网格，或 `PUT /v1/admin/quota`（`DELETE` 恢复默认） |
| 代理例外 | 代理**永不参与平台系数**：它按体积单一费率计费，改平台系数不影响它（有测试钉死） |
| 环境变量 | `VIDLINK_PROXY_RATE` 只在**文件不存在**时当种子；之后以文件为准，避免重启覆盖面板上的改动 |
| 审计 | 文件里保留最近 50 次变更（时间 / 动作 / 说明）；管理面只有一个固定 Key，所以记不到人 |
| 坏了怎么办 | 倍率文件损坏或不合法 → **拒绝启动**并指出原因（不带着半个价目表跑） |

```bash
# 抖音 links 改成 1.5，代理费率改成 0.6
curl -X PUT -H "X-API-Key: $ADMIN" -H 'Content-Type: application/json' \
  -d '{"rates":{"douyin":{"links":1.5}},"proxy_rate":0.6}' "$BASE/v1/admin/quota"
```

具体口径与字段见 [docs/API.md](docs/API.md) §3.9.5，默认值与推导见
[docs/配额倍率表.md](docs/配额倍率表.md)。**客户端应以 `/v1/usage` 返回的实时值为准**——
文档里印的是仓库默认值，部署者可能改过。

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

管理权限是**一个固定的 Key**，不是账号、也不是账号上的权限位：

- 来源：环境变量 `VIDLINK_ADMIN_KEY` 或 `.vl` 里的同名键；
- **没配置 = 管理接口整体关闭**：每条 `/v1/admin/*` 都恒返回 `403` 并说明原因，
  解析、配额、账本一切照常，只是没人能改账号；
- 它不是账本里的账号：不能用它解析视频；反过来，任何账号也都拿不到管理权限
  （账号结构里没有权限位，`{"admin":true}` 这类字段会被直接拒绝）；
- 轮换就是改这个值再重启，账本里不需要同步任何东西。

```bash
# 本机自用：随机生成一个并写进 .vl（或直接放进环境变量）
printf 'VIDLINK_ADMIN_KEY=%s\n' "$(openssl rand -hex 32)" >> .vl

# 容器化部署：不必翻日志，也不会在日志里出现
VIDLINK_ADMIN_KEY=$(openssl rand -hex 32) ./vidlink

拿到管理 Key 之后，账号管理都在 `/v1/admin/accounts` 一族接口上：`POST` 建号
（明文 Key 只在响应里出现一次）、`PATCH` 改配额/账号倍率/每日签到额度/停用、
`DELETE` 删号（路径参数可以是明文 Key，也可以是列表里的句柄 `acc_…`）。
逐字段说明与完整示例见 **[docs/API.md](docs/API.md)** §3.9；也可以直接用管理面板
（`VL_WEBUI=true` 后打开 `$BASE/admin`）——每个编辑框下面都写着它是干什么的。

**账号句柄 `id`**：列表与详情里的 `id`（形如 `acc_1f2e3d…`）是 Key 的
SHA-256 截断，不可反推、重启不变、只能用来定位账号。它的存在是为了让管理面板
能在**不接触明文 Key** 的前提下改配额/停用/删除（明文 Key 只在创建时出现一次）。

账本是 **append-only JSONL**（每次写一条完整快照 + `fsync`），重启自动回放，
损坏的行会被跳过而不是让服务起不来。默认路径 `data/accounts.jsonl`。

### 公共入口（免费试用）

服务在账户模式下会自动准备一个**公共账号**（默认 Key `vl_public`）：
Key 是公开的，谁都能用，所以它的配额不来自账本余额，而是**每 IP 每日限额**（默认 25）：

不注册也能跑通一次完整解析：带着 `X-API-Key: vl_public` 调任意解析端点即可，
响应头里的 `X-Quota-Remaining` 是**本 IP 今天**的剩余（示例见
[docs/API.md](docs/API.md) §3.8a）。

| 行为 | 说明 |
| --- | --- |
| 额度口径 | **每 IP 每日 25 配额**（`VIDLINK_PUBLIC_DAILY_QUOTA`），端点系数与其他账号相同（info 0.5 / links 1.0 / detail 1.2 / batch 0.75 每条）；代理同样是统一费率 **0.5 配额/MiB** → 每天约 25 条直链，或约 50 MiB 代理流量 |
| 用完了 | `429 public_quota_exhausted`，提示去申请独立 Key；额度跨天自动重置（本地时区零点） |
| 没带 Key | `403` 的 message 里**直接给出公共 Key**，而不是一句"缺少 API Key" |
| 账单 | 公共 Key **读不了** `/v1/ledger`（共享账号，流水混有所有访客），`/v1/usage` 返回的是"本 IP 今天"的额度 |
| 并发 | 闸门按 **IP** 而不是按 Key（Key 共享，按 Key 串行会让同时只有一个免费用户能用） |
| 统计 | 用量与调用次数仍累计到公共账号上，管理面板能看到免费流量；`/v1/admin/stats` 有 `public.ips_today` |
| 关闭它 | 在管理面板把公共账号**停用**即可（`EnsurePublic` 不会重新启用）；`VIDLINK_PUBLIC_KEY=` 留空则完全不创建 |
| 记账持久性 | 每 IP 的日计数**只在内存里**，重启即清零（持久化意味着每次计费写一次 SD 卡，不值得） |

### 每日签到（用户自助领配额）

管理员可以在任意账号上设两个属性：

| 属性 | 含义 |
| --- | --- |
| `daily_grant` | **每日签到可领的配额**（0 = 不开放签到） |
| `grant_cap` | **停止增加界限**：`余额 = min(余额 + daily_grant, grant_cap)`；0 = 不封顶 |
| `grant_day` | 最近一次签到的日期（只读，由服务记录） |

用户自己调 `POST /v1/checkin` 领取，**每天一次**（完整请求/响应见
[docs/API.md](docs/API.md) §3.8a2）：

规则：

- **只有签到会加配额**：服务**不会**在每天第一次调用时自动补额，也不会因为
  读接口/解析"顺手"给你加——加配额只发生在 `POST /v1/checkin`；
- **每天一次**：按本地日期判断（`grant_day` 随账号快照落盘，重启不丢）；
- **到界限就停**：余额 ≥ `grant_cap` 时不再增加；
- **到界限不消耗当天机会**：余额满了签到返回 `at_cap=true` 但不占名额，
  花掉一些之后当天仍可签——签到的意义就是"需要时补一点"；
- **不消耗配额**（它是来领配额的），也不占按 Key 的解析闸门；
- 每次签到写一条流水：`每日签到 +25 → 100（上限 100）`；
- 未开放签到的账号返回 `400` 并说明要找管理员设置额度；公共 Key 返回 `403`
  （它的额度是每 IP 每日自动给的，不需要签到）。

管理面板的账号卡片上有「应用签到」按钮（两个输入框：签到额度、停止增加界限），
解析页的「账户与用量」里有个常驻的「签到」按钮（在「刷新用量」旁边），签完自动刷新余额；
不可签时它变成「**强制签到**」——不绕过服务端规则，只是让服务端再确认一次，
页面上的状态可能是旧的（刚过零点、管理员刚改了额度、换了 Key 还没刷新），
所以这个按钮**永不禁用**。同区域还有一行「可否签到」，直接写清当前能不能签与原因。

### 配额流水（账单）

余额与累计用量只能回答"现在剩多少"，回答不了"这笔是怎么来的"。流水补上后者：
每一次消耗、管理员每一次增加/减少/设置都会记一条，含变化量、变化后余额与说明。

- 用户读自己的：`GET /v1/ledger?limit=20&type=consume`（服务端按 Key 限定，
  接口里没有"读别人"的入口，也不消耗配额）；
- 管理员读总账单：`GET /v1/admin/ledger`；读指定账号加 `?id=acc_…`（句柄）或 `?key=…`。

完整示例见 [docs/API.md](docs/API.md) §3.8b 与 §3.9。

| 类型 | 含义 |
| --- | --- |
| `consume` | 使用：解析或代理消耗配额（`units` 为负，`detail` 写明端点/平台或代理体积） |
| `add` / `reduce` | 管理员增加 / 减少配额 |
| `set` | 管理员设置：配额设为某值、账号倍率、停用/启用、改名等（`detail` 写明改了什么） |
| `create` / `delete` | 建号 / 删号（删号后历史流水仍保留） |

- **两个接口都不消耗配额**：对账本身收费会让人不敢对账；
- 返回 `totals` 是**全量**口径（按类型累计的次数与净变化），`entries` 只保留最近
  若干条（内存上限 5000 条，约 1 MB，更早的在账本文件里）；
- 流水与账号快照共用同一个文件、共用一次 `fsync`（SD 卡上每次 fsync 都有代价），
  重启时一起回放，因此重启不丢；
- 用户视图与管理视图里的 Key 一律掩码；管理员可以用句柄 `acc_…` 寻址。

## 图形界面

两个页面都由 `VL_WEBUI` 控制（ease 模式默认开、账户模式默认关）：

| 路径 | 页面 | 鉴权 |
|---|---|---|
| `/` | 图形化解析页（含浏览器内混流、代理下载） | 页面公开；解析与用量要账号 Key（ease 模式不需要） |
| `/admin` | **管理面板**（概览 / 建号 / 改配额改倍率 / 签到额度 / 停用删除 / 流水 / 系数表） | 页面公开；所有数据要**管理 Key**，在页面顶部填入 |

```bash
VL_WEBUI=true VIDLINK_ADMIN_KEY=... ./vidlink
# 打开 $BASE/      解析页
# 打开 $BASE/admin 管理面板（填管理 Key 后自动带上）
```

管理面板的账号卡片默认只展示信息，**操作区（改配额 / 倍率 / 签到额度 / 停用 / 删除）
默认折叠**，点一下展开；每个编辑框的说明就写在它正下方，展开状态按账号记住，
操作完不会自己收起来。

管理面板不引用任何外部资源（离线/内网可用），它发出的每个请求都会自动带上
管理 Key（内部请求走 `X-API-Key` 头；页面里的链接拼的是**现算的签名**
`adm_…`，约 30 秒有效，每 15 秒自动换新——URL 里不放明文管理 Key）。
面板只在账户模式下存在：`VL_EASE=true` 时 `/admin` 是 404，
因为那时没有账号体系可管理。

解析页里的「浏览器内混流」自带两个档位下拉与一个通道开关：

- **视频清晰度 / 音频清晰度**：在上面点一次「全部详情」（`/v1/detail`），
  两轨的全部档位会**自动填进这两个下拉**（选项文本里带编码、分辨率/码率、体积），
  选哪档就合哪档；直接点「下载并混流」时若还没取过，页面会自动取一次。
  混流用的就是这份结果，下载时不再额外计费；
- **使用服务端代理下载**：勾上则两轨都经 `/v1/proxy`（CDN 强制 Referer 时必须），
  不勾则直连、失败时按 `backup_urls` 换镜像再试。**不再自动兜底**——
  代理消耗服务端出口带宽与账号配额，用不用由使用者决定；开关状态记在本机浏览器里。

## 配置

配置有**两个来源，环境变量优先**：进程环境变量，以及可执行文件同目录
（或工作目录）下的 `.vl` 文件。**代码里不写死任何 Cookie 或密钥。**

为什么还要一个文件：有些运行环境（面板、systemd 单元、部分容器运行时、
Windows 计划任务）设环境变量会失败或悄悄丢掉，而"配置没生效"在服务端
看起来和"配置写错了"一模一样。给一个能直接编辑的文件兜底，
比让人去和运行环境搏斗划算。

```bash
# .vl —— 一行一个 KEY=VALUE；空行与 # 开头的行忽略
VL_EASE=true
VIDLINK_ADMIN_KEY=vl_admin_...        # 管理接口的固定 Key（不写就没有管理接口）
VL_WEBUI=true                         # 打开解析页与管理面板
VIDLINK_ACCOUNTS_PATH=/var/lib/vidlink/accounts.jsonl
VIDLINK_COOKIE_DOUYIN=UIFID_TEMP=...; ttwid=...
```

查找顺序（先命中先用）：

| 顺序 | 位置 | 用途 |
|---|---|---|
| 1 | `$VL_CONFIG` 指定的文件 | 部署脚本显式指定；**文件不存在会报错**，不静默退回默认值 |
| 2 | 当前工作目录 `./.vl` | 最具体：cd 进哪个目录就用哪份配置 |
| 3 | 可执行文件所在目录 `/.vl` | 全局默认：给装在 PATH 里的那个二进制定基调 |

> 为什么工作目录优先于程序目录：程序目录常是 `~/.local/bin` 这类**共享**位置，
> 放那里的 `.vl` 会对该二进制的所有调用生效；如果它优先级更高，
> 任何按目录区分的配置就永远没机会生效。

- 取值顺序：**环境变量 → `.vl` → 内置默认值**。环境变量优先是刻意的：
  临时覆盖一个值不该去改文件（`docker run -e` 更省事）；
  反过来"文件覆盖环境变量"会让排障时看到的配置与实际生效的不一致。
- 典型用法：把 `VL_EASE=true` 写进 `~/.local/bin/.vl`，此后在任意目录
  直接敲 `vidlink` 都是免校验模式；某个目录想要账户模式，就在那里放一份
  自己的 `.vl`（或临时 `VL_EASE=false vidlink`）。
- 格式宽容：支持 `export KEY=VALUE`、`KEY="VALUE"`、等号两侧空格、CRLF、BOM；
  坏行会被跳过而不是让服务起不来。值里可以含 `=`（Cookie 常见）。
- **`.vl` 已在 `.gitignore` 与 `.dockerignore` 里**——它可能含 Cookie 与管理员 Key。
  容器里请挂载：`-v /host/vidlink.vl:/app/.vl:ro`。
- 启动日志会打印实际生效的来源（`config=环境变量` 或 `config=/path/.vl`），
  用来分辨"文件路径不对"与"被环境变量覆盖了"。

### 全部变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `VL_EASE` | `false` | **免校验模式**：账户/配额/鉴权整体关闭，只留解析（见上一节） |
| `VL_WEBUI` | 跟随模式（ease `true` / 账户 `false`） | 根路径是否返回**图形化页面**；显式设置两个方向都有效 |
| `VIDLINK_ADDR` | `:8080` | 监听地址 |
| `VIDLINK_ADMIN_KEY` | 空 | **管理接口的固定凭据**；留空 = `/v1/admin/*` 恒 403（没人能改账号），解析不受影响 |
| `VIDLINK_PUBLIC_KEY` | `vl_public` | 公共账号的 Key（公开、免注册试用）；**留空 = 不提供公共入口** |
| `VIDLINK_PUBLIC_DAILY_QUOTA` | `25` | 公共账号**每个 IP 每天**的配额；用完了 `429`，跨天自动重置 |
| `VIDLINK_SIG_TTL` | `30s` | 签名凭据的时间容差与有效期（`5s`~`1h`）；URL 里只收签名，见 API 文档 §2.2 |
| `VIDLINK_PROXY_RATE` | `0.5` | 媒体代理费率的**首次种子**（配额/MiB，所有账号同价）；文件存在后以文件为准 |
| `VIDLINK_RATES_PATH` | `data/rates.json` | 计费倍率覆盖层的落盘路径（原子写）；留空 = 纯内存，重启即丢 |
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
| `VIDLINK_PROXY_ENDPOINT` | `false`（ease 模式下 `true`） | 是否开启媒体代理；显式 `false` 可关掉 |
| `VIDLINK_PROXY_ALLOW_HOSTS` | 空 | 媒体代理域名后缀白名单；**留空 = 不限制**（等同于开放代理，勿暴露公网）。条目会被规范化（可写 `*.x.com` 或完整直链），`*`/单标签等条目被丢弃并在启动日志里列出 |
| `VIDLINK_TRUSTED_PROXY_HEADER` | 空 | 如 `X-Forwarded-For`，用于取真实客户端 IP |
| `VIDLINK_PPROF` | `false` | 开启 pprof（仅 `127.0.0.1:6060`） |

> ⚠️ **媒体代理等同于一个 HTTP 代理**。账户模式默认关闭；免校验模式默认开启
> （浏览器内混流遇到要求 Referer 的 CDN 节点时要靠它）。
> `VIDLINK_PROXY_ALLOW_HOSTS` 留空表示放行任意目标，此时**绝不能暴露到公网**——
> 不想开就把 `VIDLINK_PROXY_ENDPOINT=false` 显式写上。

白名单的写法很宽容，但**不会**接受能匹配一切的条目：

```bash
# 下面三种写法等价，都会被规范化成 upos-sz-mirror08c.bilivideo.com
VIDLINK_PROXY_ALLOW_HOSTS=upos-sz-mirror08c.bilivideo.com
VIDLINK_PROXY_ALLOW_HOSTS=*.bilivideo.com
VIDLINK_PROXY_ALLOW_HOSTS=https://upos-sz-mirror08c.bilivideo.com/
```

- 匹配按 DNS 标签后缀：`bilivideo.com` 命中 `upos-sz-mirror08c.bilivideo.com`，
  不命中 `notbilivideo.com`、也不命中 `bilivideo.com.evil.cn`；
- `*`、`com`、`localhost`、含非法字符或超长的条目会被**丢弃**并打印 WARN
  （静默地"只生效一半"比直接报错更难查）；
- 生效的白名单会以 `allow_hosts=...` 打在启动日志里。

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
  -e VIDLINK_ADMIN_KEY="$(openssl rand -hex 32)" \  # 记住它：这是唯一的管理凭据
  vidlink:latest
# 抖音需要访客身份时，再把 VIDLINK_COOKIE_DOUYIN 传进来（本地铸造，见上文）
```

**务必挂载 `/app/data`**：账本在那里，不挂载的话容器一重建，所有账号与配额就没了。

## 项目结构

```
vidlink/
├── main.go                          入口：装配 + 优雅关闭 + 探针模式
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
│   │   └── credential.go            签名凭据：句柄派生 / 签发 / 定长解析 / 校验
│   ├── gate/                        两道并发闸门（按 Key + 全局）
│   └── server/                      HTTP 路由 / 中间件 / 管理面 / 流式代理
│       ├── ui.html                  图形化解析页（内嵌进二进制）
│       ├── admin.html               管理面板（内嵌进二进制）
│       └── auth.go                  凭据解析：明文 Key / 签名 / 管理面
├── examples/                        客户端示例（零依赖，随仓库发布、有测试跑）
│   ├── sign.py   sign.js   sign.sh  生成签名凭据
│   └── parse.py  parse.js  parse.sh 完整解析客户端（解析 / 用量 / 代理下载）
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

- [**API 文档**](docs/API.md) — **唯一的人读接口文档**：全部端点、示例（相对路径）、
  错误码、公共 Key、每日签到、配额流水、可编辑倍率、媒体代理
- 在线版本：<https://github.com/wzmwayne/vidlink/blob/master/docs/API.md>
- 官方测试实例：<https://vl.wzml.cc.cd>（公共 Key `vl_public` 免注册可试；
  它跑在一台树莓派上，可能限流或随时变动，**正式使用请自部署**）
- [OpenAPI 3.0 规格](docs/openapi.yaml) — 接口契约（可直接生成客户端 SDK）
- [配额倍率表](docs/配额倍率表.md) — 每个系数是多少、为什么、怎么改
- [配额说明（用户版）](docs/配额说明-用户版.md) — 面向调用方的配额速查
- [平台调研报告](docs/平台调研报告.md) — 四平台的接口、签名、水印机制、风控与实测结论
- [架构设计](docs/架构设计.md) — 设计决策、取舍与演进路线
