// Package config 负责把环境变量转成运行期配置。
//
// 原则：**不在代码里写死任何密钥、Cookie 或账号**。所有敏感与易变的东西
// 都通过环境变量注入，既满足十二要素应用，也便于容器化部署时轮换。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/publicq"
	"vidlink/internal/service"
)

// Config 是完整运行期配置。
type Config struct {
	Addr string

	// Ease 是"免校验模式"：由 VL_EASE=true 打开。
	//
	// 打开后**账户体系整体关闭**：不建账本、不需要 API Key、不计量配额、
	// 没有管理接口，只保留公开端点与四个解析端点。
	//
	// 用途是本地/内网把服务当纯解析工具用（比如自己写脚本、塞进别的程序），
	// 不必为了取一条直链先去建账号、发 Key、算配额。
	//
	// 安全性由部署方式来保证：这个模式没有任何身份校验，
	// **绝不能直接暴露到公网**（见 README 的警告）。
	Ease bool

	// ConfigFile 是实际生效的 .vl 文件路径（没有则为空串）。
	//
	// 只用于启动日志：配置文件这类东西必须可观测——
	// "我改了文件但没生效"最常见的两个原因是路径不对与被环境变量覆盖，
	// 把路径和键数打出来，一眼就能分辨。
	ConfigFile string

	// WebUI 控制根路径是否返回图形化解析页（VL_WEBUI）。
	//
	// 默认值跟随运行模式：免校验模式默认开（那本来就是"自用工具"的场景），
	// 账户模式默认关（常部署在公网，不该默认多一个匿名可达的界面）；
	// 显式设置两个方向都有效。
	//
	// 账户模式下打开它时，页面本身是公开的（否则浏览器拿不到页面、也就
	// 没地方填 Key），但页面上的一切数据仍要 Key——用量、解析、代理都一样。
	WebUI bool

	// AdminKey 是管理面（/v1/admin/*）的**固定凭据**，来自环境变量
	// VIDLINK_ADMIN_KEY 或 .vl 里的同名键。
	//
	// 它不是账号、也不是账号上的权限位：账本里的 Key 无论怎么改都拿不到
	// 管理权限，这个 Key 也不能用来解析视频（它不在账本里）。
	// 二者唯一的交集是"客户端用同一种方式提交"
	// （X-API-Key 头 / Authorization: Bearer / ?key=）。
	//
	// 为空表示**管理接口整体关闭**：每条 /v1/admin/* 都恒返回 403，
	// 服务照常运行（解析、配额、账本都不受影响），只是没人能改账号。
	// 刻意不做"自动生成一个管理员"这类引导：那会让"管理权限从哪来"
	// 变成一个必须读代码才能回答的问题。
	//
	//	docker run -e VIDLINK_ADMIN_KEY=$(openssl rand -hex 32) ...
	//
	// 轮换就是改这个值再重启，账本里不需要同步任何东西。
	AdminKey string

	// PublicKey 是**公共账号**的 Key（默认 vl_public），留空表示不提供公共入口。
	//
	// 公共账号的 Key 是公开的，谁都能用，所以它的配额不来自账本余额，
	// 而是"每 IP 每日限额"（见 PublicDailyQuota）。它是引流/试用的入口：
	// 不注册也能跑通一次完整解析，用完了要么等明天，要么找管理员要独立 Key。
	//
	// 服务启动时按这个值确保账号存在；已存在时**不动它**——管理员把公共账号
	// 停用就是"关闭公共入口"的开关（见 account.EnsurePublic）。
	PublicKey string

	// PublicDailyQuota 是公共账号每个 IP 每天的配额（默认 100）。
	//
	// 口径与其他端点一致：info 0.5 / links 1.0 / detail 1.2 /
	// batch 0.75 每条 / 代理 1 配额每 MiB。也就是默认每天能取约 100 条直链，
	// 或约 100 MB 代理流量。
	PublicDailyQuota float64

	// RateLimitRPM 是每 IP 每分钟请求上限；<=0 表示不限。
	RateLimitRPM int

	// CORSOrigins 为 "*" 表示允许所有来源。
	CORSOrigins []string

	// Proxy 是上游代理地址，形如 http://host:port。
	Proxy string

	// Cookies 按平台注入登录态。绝不在代码中硬编码。
	Cookies map[core.Platform]string

	// TrustedProxyHeader 允许从该头读取真实客户端 IP（如 X-Forwarded-For）。
	TrustedProxyHeader string

	// EnablePprof 开启 /debug/pprof。
	EnablePprof bool

	// AccountsPath 是账号账本（JSONL）的落盘路径。
	// 为空则纯内存，重启即丢——只适合测试。
	AccountsPath string

	// PerKeyConcurrency / GlobalConcurrency / QueueMax / QueueWaitTimeout
	// 是两道并发闸门的参数，见 gate 包注释。
	PerKeyConcurrency int
	GlobalConcurrency int
	QueueMax          int
	QueueWaitTimeout  time.Duration

	// BatchMin / BatchMax 是批量接口的条数范围。
	//
	// 下限设成 5 而不是 1，是为了让"批量更省配额"只给真正的批量：
	// 否则客户拿 1 条也能省 25% 配额，这个下调就失去意义。
	BatchMin int
	BatchMax int

	// Service / Net 是下层可调参数。
	Service  service.Options
	Net      NetOptions
	ProxySrv ProxyOptions
}

// NetOptions 是 HTTP 客户端参数。
type NetOptions struct {
	Timeout             time.Duration
	MaxIdleConnsPerHost int
	RatePerSecond       float64
	Burst               int
	Retries             int
}

// ProxyOptions 是媒体代理参数。
type ProxyOptions struct {
	// Enabled 是否挂载 /api/v1/proxy。
	Enabled bool
	// MaxBytes 单次代理的最大字节数（0 表示不限）。
	MaxBytes int64
	// AllowHosts 允许代理的目标域名后缀白名单（**已规范化**，见
	// normalizeHostSuffixes：只保留裸域名，小写、无通配符、无端口、无路径）。
	//
	// **为空表示不限制**（允许任意 http/https 目标）。这是刻意的默认：
	// 打开代理这件事本身就是"我要用它取流"，再强制填一份域名清单
	// 只会让人随手写个通配符，既不安全也不省事。
	// 想收紧就填具体后缀，例如 bilivideo.com（写成 *.bilivideo.com 或
	// https://upos-sz-mirror08c.bilivideo.com/ 也会被规范化成同一个后缀）。
	//
	// 匹配规则是**按 DNS 标签的后缀**（urlx.HostHasSuffix）：
	// bilivideo.com 命中 upos-sz-mirror08c.bilivideo.com，
	// 但不会命中 notbilivideo.com，也不会命中 bilivideo.com.evil.cn。
	AllowHosts []string

	// RejectedHosts 是白名单里**没通过校验**因而被丢弃的条目（原样保存）。
	//
	// 它们不会让服务起不来（与 .vl 的容错取向一致），但必须在启动日志里
	// 露出来：白名单被静默地"部分生效"是运维最容易被骗过去的一种状态——
	// 表现为"我明明填了，怎么还是 403"或者更糟"以为收紧了其实没生效"。
	RejectedHosts []string
}

// normalizeHostSuffixes 把白名单条目规范化成"裸域名后缀"，并挑出非法项。
//
// 为什么要规范化：白名单是人手写的，实际见到的形态五花八门——
// 直接抄一条直链（https://upos-sz-mirror08c.bilivideo.com/）、带通配符
// （*.bilivideo.com）、带端口、大写、结尾多一个点、同一项写两遍。
// 这些都不该让服务起不来，但**也绝不能原样拿去做后缀匹配**，
// 那样结果是"填了等于没填"，而且很难看出来。
//
// 拒绝的条件（宁可丢弃这一条并告警，也不接受能"匹配一切"的条目）：
//
//   - 空、纯点号、含 `*`（开头的 `*.` 除外）、含 `://` 残留；
//   - 单标签（`com`、`localhost`）——那等于放行整个顶级域或本机名；
//   - 非法字符（只允许 a-z0-9- 与 `.`，且每段不能以 `-` 开头/结尾）；
//   - 超过 DNS 上限（单段 63、总长 253）。
//
// 返回 (可用后缀, 被拒条目)，两者都保持输入顺序并去重。
func normalizeHostSuffixes(in []string) (ok, bad []string) {
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		host := strings.ToLower(entry)

		// 抄直链是常见写法：去掉 scheme、userinfo、路径、查询、片段。
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		host = strings.TrimPrefix(host, "//")
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if i := strings.LastIndex(host, "@"); i >= 0 { // user:pass@host
			host = host[i+1:]
		}
		// 端口：只在"冒号后全是数字"时剥掉（IPv6 字面量会在下面的字符校验里被拒）
		if i := strings.LastIndex(host, ":"); i >= 0 && allDigits(host[i+1:]) {
			host = host[:i]
		}
		host = strings.TrimPrefix(host, "*.") // 通配符只是写法，后缀匹配天然含子域
		host = strings.Trim(host, ".")

		if !validHostSuffix(host) {
			bad = append(bad, entry)
			continue
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		ok = append(ok, host)
	}
	return ok, bad
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validHostSuffix 校验一个已经规范化的域名后缀。
func validHostSuffix(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 { // 单标签（com / localhost）会放行一整片，直接拒
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return false
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// Load 从环境变量读取配置并填充默认值。
func Load() (*Config, error) {
	// 先把同目录 .vl 读进来，后面所有 env* 取值都自动获得"文件兜底"能力。
	kv, path, err := findDotVL()
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	// 显式指定的路径不存在时必须报错：那说明部署脚本写错了，
	// 而"默默按默认值跑起来"会让人以为配置生效了。
	if want := strings.TrimSpace(os.Getenv(VL_CONFIG_KEY)); want != "" && path == "" {
		return nil, fmt.Errorf("config: %s 指定的文件不存在: %s", VL_CONFIG_KEY, want)
	}
	dotVL = kv

	// 免校验模式要先算出来：媒体代理的**默认开关跟随它**——
	// ease 是"本机/内网自用"，浏览器内混流遇到需要 Referer 的 CDN 节点时
	// 必须能走代理，默认关着会让那个功能时灵时不灵。
	ease := envBool("VL_EASE", false)

	// 代理白名单先规范化：允许人手写直链或通配符，但**不允许**能匹配一切的
	// 条目悄悄生效（那就是"以为收紧了，其实没有"）。
	allowHosts, badHosts := normalizeHostSuffixes(splitList(env("VIDLINK_PROXY_ALLOW_HOSTS", "")))

	c := &Config{
		ConfigFile: path,
		Addr:       env("VIDLINK_ADDR", ":8080"),
		Ease:       ease,
		// 与媒体代理同样的思路：默认值跟随模式，显式设置优先。
		WebUI:              envBool("VL_WEBUI", ease),
		AdminKey:           env("VIDLINK_ADMIN_KEY", ""),
		PublicKey:          env("VIDLINK_PUBLIC_KEY", "vl_public"),
		PublicDailyQuota:   envFloat("VIDLINK_PUBLIC_DAILY_QUOTA", publicq.DefaultDaily),
		RateLimitRPM:       envInt("VIDLINK_RATE_LIMIT_RPM", 120),
		CORSOrigins:        splitList(env("VIDLINK_CORS_ORIGINS", "*")),
		Proxy:              env("VIDLINK_PROXY", ""),
		TrustedProxyHeader: env("VIDLINK_TRUSTED_PROXY_HEADER", ""),
		EnablePprof:        envBool("VIDLINK_PPROF", false),
		// 账本落盘路径。默认写到 ./data/accounts.jsonl：
		// 纯内存意味着重启后所有账号与配额凭空消失，不能作为默认行为。
		AccountsPath: env("VIDLINK_ACCOUNTS_PATH", "data/accounts.jsonl"),
		Cookies:      map[core.Platform]string{},
		Service: service.Options{
			ParseTimeout: envDuration("VIDLINK_PARSE_TIMEOUT", 20*time.Second),
			CacheTTL:     envDuration("VIDLINK_CACHE_TTL", 10*time.Minute),
			NegativeTTL:  envDuration("VIDLINK_NEGATIVE_TTL", 45*time.Second),
			// 默认 3：全局解析槽位是 10，3 意味着同时约 3 个批量请求在跑，
			// 单个批量不至于把全局槽位一次吃光。
			BatchConcurrency: envInt("VIDLINK_BATCH_CONCURRENCY", 3),
		},
		Net: NetOptions{
			Timeout:             envDuration("VIDLINK_HTTP_TIMEOUT", 12*time.Second),
			MaxIdleConnsPerHost: envInt("VIDLINK_MAX_IDLE_PER_HOST", 32),
			RatePerSecond:       envFloat("VIDLINK_UPSTREAM_RPS", 8),
			Burst:               envInt("VIDLINK_UPSTREAM_BURST", 16),
			Retries:             envInt("VIDLINK_HTTP_RETRIES", 2),
		},
		BatchMin: envInt("VIDLINK_BATCH_MIN", 5),
		BatchMax: envInt("VIDLINK_BATCH_MAX", 20),
		// 两道并发闸门。默认：每 Key 串行、全局 10 路解析、最多排 30 个等 15 秒。
		PerKeyConcurrency: envInt("VIDLINK_PER_KEY_CONCURRENCY", 1),
		GlobalConcurrency: envInt("VIDLINK_GLOBAL_CONCURRENCY", 10),
		QueueMax:          envInt("VIDLINK_QUEUE_MAX", 30),
		QueueWaitTimeout:  envDuration("VIDLINK_QUEUE_WAIT_TIMEOUT", 15*time.Second),
		ProxySrv: ProxyOptions{
			// 默认值 = 是否处于免校验模式；显式设置 VIDLINK_PROXY_ENDPOINT
			// （true/false）时以显式值为准。"显式 false 要能关掉"是这条的硬要求，
			// 所以用 envBool 的默认值参数，而不是"设了 true 才开"。
			Enabled:       envBool("VIDLINK_PROXY_ENDPOINT", ease),
			MaxBytes:      int64(envInt("VIDLINK_PROXY_MAX_MB", 0)) << 20,
			AllowHosts:    allowHosts,
			RejectedHosts: badHosts,
		},
	}

	// Cookie 注入。命名约定：VIDLINK_COOKIE_<PLATFORM>
	for _, p := range []core.Platform{
		core.PlatformDouyin, core.PlatformBilibili,
		core.PlatformKuaishou, core.PlatformXiaohongshu,
	} {
		key := "VIDLINK_COOKIE_" + strings.ToUpper(string(p))
		if v := env(key, ""); v != "" {
			c.Cookies[p] = v
		}
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// IsEase 报告是否处于免校验模式。
//
// 做成方法而不是到处读 c.Ease：这个开关决定"账户体系是否存在"，
// 调用点应当一眼看出自己在问什么。
func (c *Config) IsEase() bool { return c != nil && c.Ease }

func (c *Config) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("config: VIDLINK_ADDR 不能为空")
	}
	if c.Service.BatchConcurrency < 1 {
		c.Service.BatchConcurrency = 1
	}
	if c.Net.MaxIdleConnsPerHost < 1 {
		c.Net.MaxIdleConnsPerHost = 8
	}
	// 闸门参数为零值会被 gate 的内部默认值兜住，但排队上限 0 是**合法**取值
	// （表示不排队），所以只能在这里给出默认，不能让 normalize 去猜。
	if c.PerKeyConcurrency < 1 {
		c.PerKeyConcurrency = 1
	}
	if c.GlobalConcurrency < 1 {
		c.GlobalConcurrency = 10
	}
	if c.QueueMax < 0 {
		c.QueueMax = 30
	}
	if c.QueueWaitTimeout <= 0 {
		c.QueueWaitTimeout = 15 * time.Second
	}
	if c.BatchMin < 1 {
		c.BatchMin = 1
	}
	if c.BatchMax < c.BatchMin {
		return fmt.Errorf("config: VIDLINK_BATCH_MAX(%d) 不能小于 VIDLINK_BATCH_MIN(%d)",
			c.BatchMax, c.BatchMin)
	}
	// 免校验模式就是"不要任何请求级拦截"，按 IP 的限流也一并关掉——
	// 否则会出现"不需要 Key，但打快一点就被 429"这种半吊子状态，
	// 而那个 429 与身份、配额都无关，用户根本无从理解。
	if c.Ease {
		c.RateLimitRPM = 0
	}
	if c.Net.Burst < 1 {
		c.Net.Burst = 4
	}
	return nil
}

// ApplyCookies 把配置里的 Cookie 灌进 deps 仓库。
func (c *Config) ApplyCookies(store *deps.Cookies) {
	for p, v := range c.Cookies {
		store.Set(p, v)
	}
}

// --- 环境变量小工具 ---
//
// 取值顺序：**进程环境变量优先，同目录的 .vl 文件兜底**。
//
// 为什么要文件：有些运行环境（面板、systemd 单元、部分容器运行时、
// Windows 计划任务）设环境变量会失败或悄悄丢掉，而"配置没生效"这件事
// 在服务端看起来和"配置写错了"一模一样。给一个可直接编辑的文件兜底，
// 比让人去和运行环境搏斗划算。
//
// 文件格式刻意做得极简——就是 KEY=VALUE 逐行，忽略空行与 # 开头的注释：
//
//	# /opt/vidlink/.vl
//	VL_EASE=true
//	VIDLINK_ACCOUNTS_PATH=/var/lib/vidlink/accounts.jsonl
//
// 环境变量优先是刻意的：临时覆盖一个值不该去改文件（docker run -e 更省事），
// 反过来"文件覆盖环境变量"会让排障时看到的配置与实际生效的不一致。

// dotVL 是 .vl 文件里读到的键值对，只在 Load 期间被填充。
//
// 用包级变量是为了让下面这些 env* 帮助函数保持原样的签名——
// 它们有二十多个调用点，为了传一个 map 而全改一遍不值得。
// Load 不是并发入口（进程启动时调一次），因此不需要加锁。
var dotVL = map[string]string{}

// lookupEnv 按"环境变量 → .vl 文件"的顺序取一个非空值。
func lookupEnv(key string) (string, bool) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(dotVL[key]); v != "" {
		return v, true
	}
	return "", false
}

// VL_CONFIG_KEY 是指定配置文件路径的变量名（只能来自进程环境变量）。
const VL_CONFIG_KEY = "VL_CONFIG"

// findDotVL 找出生效的 .vl，返回解析结果与路径。
//
// 候选顺序（先命中先用）：
//
//  1. $VL_CONFIG 指定的文件      —— 显式指定，写错路径要报错而不是静默忽略
//  2. 当前工作目录/.vl           —— 最具体：cd 进哪个目录就用哪份配置
//  3. 可执行文件所在目录/.vl      —— 兜底：给"装在 PATH 里的那个二进制"设全局默认
//
// 为什么工作目录优先于程序目录：程序目录常常是 ~/.local/bin 这类**共享**位置，
// 放在那里的 .vl 会对这个二进制的所有调用生效；如果它优先级更高，
// 任何按目录区分的配置（例如某项目想用账户模式）就永远没机会生效。
// 反过来，程序目录的 .vl 依然能作为全局默认兜住"在任意目录直接敲 vidlink"。
func findDotVL() (map[string]string, string, error) {
	var candidates []string
	if p := strings.TrimSpace(os.Getenv(VL_CONFIG_KEY)); p != "" {
		candidates = append(candidates, p)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, ".vl"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".vl"))
	}
	return firstDotVL(candidates)
}

// firstDotVL 返回候选路径里第一个存在且可读的配置。
func firstDotVL(candidates []string) (map[string]string, string, error) {
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			// 文件存在但读不了（权限、是目录）必须报出来：
			// 静默忽略会让人以为"文件里写的都生效了"。
			return nil, path, err
		}
		return parseDotVL(data), path, nil
	}
	return map[string]string{}, "", nil
}

// parseDotVL 解析 .vl 文件内容。
//
// 容错取向：**坏行跳过，不报错**。这个文件的定位是"环境变量的替代品"，
// 里面通常还有人手写的注释与临时注释掉的行；因为一行写错就让服务起不来，
// 代价比忽略它大得多。真正拼错的键名会在启动日志的配置摘要里露出来。
func parseDotVL(data []byte) map[string]string {
	out := map[string]string{}
	text := strings.TrimPrefix(string(data), "\ufeff") // 编辑器可能带 BOM
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 允许写成 shell 风格：export VL_EASE=true
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue // 没有 = 的行忽略（注释写漏 # 时不至于把整份配置带偏）
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// 去掉成对的引号：VL_EASE="true" 也能用
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		out[key] = val
	}
	return out
}

func env(key, def string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := lookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v, ok := lookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(key string, def bool) bool {
	raw, ok := lookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := lookupEnv(key)
	if !ok {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	// 纯数字按秒解释
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DefaultUserAgent 返回媒体代理使用的默认 UA。
func (n NetOptions) DefaultUserAgent() string {
	return deps.DefaultParams().Desktop()
}
