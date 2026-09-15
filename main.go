// Command vidlink 是一个极轻量、高并发、零第三方依赖的多平台视频解析 API 服务。
//
// 支持平台：抖音、哔哩哔哩、快手、小红书。
//
// 设计要点：
//   - **零第三方依赖**：只用 Go 标准库，二进制小、启动快、无 CGO；
//   - **不代理视频字节**（默认）：API 只返回 CDN 直链，服务端几乎不耗带宽与内存；
//   - **同请求合并 + TTL 缓存**：热点视频的 N 个并发请求只打上游 1 次；
//   - **按主机限流**：保护上游（防风控），也保护自己（防雪崩）；
//   - **可插拔提取器**：新增平台不改服务层与路由层。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // 仅在开启时通过独立 mux 暴露
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/config"
	"vidlink/internal/deps"
	"vidlink/internal/extract"
	"vidlink/internal/gate"
	"vidlink/internal/netx"
	"vidlink/internal/quota"
	"vidlink/internal/server"
	"vidlink/internal/service"
	"vidlink/internal/sign/abogus"
	"vidlink/internal/sign/wbi"
)

// buildVersion 可被 -ldflags "-X main.buildVersion=..." 覆盖。
var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vidlink: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	hcAddr := flag.String("healthcheck", "",
		"探针模式：请求该地址的 /healthz 后退出，成功则退出码 0。"+
			"供容器 HEALTHCHECK 使用（scratch 镜像里没有 shell 与 curl）。"+
			"留空则不启用；传 -healthcheck=auto 表示用 VIDLINK_ADDR 推导。")
	flag.Parse()
	if *showVersion {
		fmt.Printf("vidlink %s (%s/%s, %s)\n", buildVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	server.Version = buildVersion

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 探针模式要在建立监听之前返回：它是另一个进程（容器运行时拉起的一次性调用），
	// 不能顺带把服务端口占了。
	if *hcAddr != "" {
		addr := *hcAddr
		if addr == "auto" {
			addr = cfg.Addr
		}
		return runHealthcheck(addr)
	}

	// 1) 基础设施
	client, err := netx.New(netx.Options{
		Timeout:             cfg.Net.Timeout,
		MaxIdleConnsPerHost: cfg.Net.MaxIdleConnsPerHost,
		Proxy:               cfg.Proxy,
		RatePerSecond:       cfg.Net.RatePerSecond,
		Burst:               cfg.Net.Burst,
		Retries:             cfg.Net.Retries,
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTP 客户端失败: %w", err)
	}

	cookies := deps.NewCookies()
	cfg.ApplyCookies(cookies)

	d := &deps.Deps{
		Client:      client,
		WBI:         wbi.NewManager(client),
		ABogus:      abogus.New(),
		Cookies:     cookies,
		Params:      deps.DefaultParams(),
		ResultCache: nil, // 平台内部缓存按需开启
		Now:         time.Now,
	}

	// 2) 平台提取器（新增平台只需在这里加一行）
	reg := extract.NewRegistry(d)

	// 3) 业务服务
	svc := service.New(d, reg, cfg.Service)

	// 4) 账本（配额 + 账号倍率 + 用量）
	//
	// 免校验模式（VL_EASE=true）下**整个账户体系都不建立**：不读也不写账本文件、
	// 不创建管理员。这才是"所有有关账户的内容关闭"应有的样子——
	// 留一个空账本在那里，早晚会有人以为它在生效。
	var accounts *account.Store
	if cfg.IsEase() {
		logger.Warn("免校验模式已开启（VL_EASE=true）：不校验 API Key、不计量配额、" +
			"无管理接口；请勿将本服务直接暴露到公网")
	} else {
		var err error
		accounts, err = account.New(account.Options{Path: cfg.AccountsPath})
		if err != nil {
			return fmt.Errorf("初始化账本失败: %w", err)
		}
		defer func() { _ = accounts.Close() }()

		// 冷启动：账本里没有管理员时自动创建一个。
		// 否则第一次部署会陷入"没有管理员 → 无法创建账号 → 永远没有管理员"。
		adminKey, err := ensureAdmin(accounts, logger, cfg.AdminKey)
		if err != nil {
			return err
		}
		if adminKey != "" {
			logger.Warn("已创建初始管理员账号，请立即保存这个 Key（只显示这一次）",
				"admin_key", adminKey)
		}
	}

	// 5) 并发闸门：默认每 Key 1 个请求、全局 10 个解析任务
	gateOpts := gate.Options{
		PerKey:      cfg.PerKeyConcurrency,
		Global:      cfg.GlobalConcurrency,
		MaxQueue:    cfg.QueueMax,
		WaitTimeout: cfg.QueueWaitTimeout,
	}

	// 6) HTTP 层
	srv, err := server.New(cfg, server.Deps{
		Service:    svc,
		Accounts:   accounts,
		Gate:       gate.New(gateOpts),
		QuotaTable: quota.DefaultTable(),
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 媒体代理是流式的，写超时不能太短；0 表示不限制
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// 5) 可选的 pprof（独立端口，避免与业务端口混在一起）
	var pprofSrv *http.Server
	if cfg.EnablePprof {
		pprofSrv = &http.Server{Addr: "127.0.0.1:6060", Handler: http.DefaultServeMux}
		go func() {
			logger.Info("pprof 已开启（仅本机）", "addr", pprofSrv.Addr)
			_ = pprofSrv.ListenAndServe()
		}()
	}

	logStartup(logger, cfg, reg)

	// 6) 启动与优雅退出
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("监听 %s 失败: %w", cfg.Addr, err)
			return
		}
		errCh <- nil
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case sig := <-quit:
		logger.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pprofSrv != nil {
		_ = pprofSrv.Shutdown(ctx)
	}
	if err := httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("关闭超时: %w", err)
	}
	logger.Info("已退出")
	return nil
}

// runHealthcheck 请求本地 /healthz，成功返回 nil。
//
// 它存在的理由是 scratch 镜像里既没有 shell 也没有 curl，
// 容器的 HEALTHCHECK 只能由二进制自己完成。
func runHealthcheck(addr string) error {
	// VIDLINK_ADDR 形如 ":8080" 或 "0.0.0.0:8080"，都要收敛成可拨号的地址。
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("探针地址 %q 无效: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("探针请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("探针返回 %d", resp.StatusCode)
	}
	return nil
}

func logStartup(logger *slog.Logger, cfg *config.Config, reg *extract.Registry) {
	platforms := make([]string, 0, 4)
	authed := make([]string, 0, 4)
	for _, e := range reg.All() {
		platforms = append(platforms, string(e.Name()))
		if cfg.Cookies[e.Name()] != "" {
			authed = append(authed, string(e.Name()))
		}
	}
	mode := "账户模式"
	if cfg.IsEase() {
		mode = "免校验模式"
	}
	logger.Info("vidlink 启动",
		"version", buildVersion,
		"addr", cfg.Addr,
		"mode", mode,
		"platforms", fmt.Sprint(platforms),
		"cookie_configured", fmt.Sprint(authed),
		"cache_ttl", cfg.Service.CacheTTL.String(),
		"proxy_endpoint", cfg.ProxySrv.Enabled,
	)
	switch {
	case cfg.IsEase():
		// 免校验模式下账户参数一律无意义，说了只会误导
	case cfg.AccountsPath == "":
		logger.Warn("未配置 VIDLINK_ACCOUNTS_PATH：账本只在内存里，重启后账号与配额全部丢失")
	default:
		logger.Info("账户模式已启用", "accounts_path", cfg.AccountsPath,
			"admin_key_configured", cfg.AdminKey != "")
	}
}

// ensureAdmin 在账本里没有任何启用的管理员时创建一个，并返回它的明文 Key。
// 已经有管理员则返回空串（此时 wantKey 被忽略）。
//
// 为什么需要它：全新的部署没有任何账号，而创建账号的接口本身又需要
// 管理员权限——不自动引导就会死锁。Key 只在创建时打印一次，
// 之后所有接口都只返回掩码。
//
// wantKey 非空时用它作为初始 Key（便于部署自动化），为空则随机生成 256 位。
func ensureAdmin(store *account.Store, logger *slog.Logger, wantKey string) (string, error) {
	for _, a := range store.List() {
		if a.Admin && !a.Disabled {
			return "", nil
		}
	}

	key := strings.TrimSpace(wantKey)
	generated := false
	if key == "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("生成管理员 Key 失败: %w", err)
		}
		key = "vl_admin_" + hex.EncodeToString(b[:])
		generated = true
	}

	// 这个 Key 可能已经作为普通账号存在（比如上一次部署手工建过）：
	// 直接提升为管理员，比报"账号已存在"然后卡在无管理员状态要好。
	if _, ok := store.Get(key); ok {
		if _, err := store.Update(key, account.Patch{
			Admin: boolPtr(true), Disabled: boolPtr(false),
		}); err != nil {
			return "", fmt.Errorf("提升已有账号为管理员失败: %w", err)
		}
		logger.Info("账本中没有启用的管理员，已把指定 Key 的账号提升为管理员")
		return key, nil
	}

	if _, err := store.Create(account.Account{
		Key:        key,
		Name:       "初始管理员",
		Admin:      true,
		Quota:      initialAdminQuota,
		Multiplier: 1,
		Note:       "由服务自动创建；可改配额与账号倍率",
	}); err != nil {
		return "", fmt.Errorf("创建管理员失败: %w", err)
	}
	if generated {
		logger.Info("账本中没有管理员，已创建初始管理员（随机 Key）")
	} else {
		logger.Info("账本中没有管理员，已用 VIDLINK_ADMIN_KEY 创建初始管理员")
	}
	return key, nil
}

func boolPtr(b bool) *bool { return &b }

// initialAdminQuota 是自动创建的初始管理员的配额。
//
// 不能给 0：全新部署时操作者手里只有这一个 Key，配额 0 会让它连
// 一个计量端点都调不动，必须先去管理接口给自己改配额——第一次使用
// 就卡住，是纯粹的摩擦。
const initialAdminQuota = 1_000_000
