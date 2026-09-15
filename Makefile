# vidlink 常用开发与运维命令。
#
# 设计意图：把所有"应该跑哪些检查"固化下来。CI 与本地用同一组目标，
# 避免"本地过了 CI 挂"这类只由命令差异造成的失败。

BINARY  := vidlink
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

# 交叉编译目标清单，格式：GOOS/GOARCH[/GOARM]
#
# 只列真正会被用到的组合：Linux（arm32 / arm64 / x86_32 / amd64）与
# Windows（x86_32 / amd64 / arm64）。BSD、Solaris、AIX、mips、riscv64
# 这些"能编但没人拿来跑这个服务"的目标不列——每多一个目标就多一份
# 需要在 CI 里长期维护的构建时间。
#
# arm32 分两档，因为树莓派横跨两代指令集：
#   armv7 (GOARM=7)  Pi 2/3/4/5、Zero 2 W、绝大多数 ARM 路由/NAS
#   armv6 (GOARM=6)  Pi 1、Zero（第一代）、更老的 ARMv6 设备
#
# 关于 Windows 7：Go 1.21 起官方已放弃 Windows 7/8（要求 Windows 10+），
# 而本项目又依赖 Go 1.22 的 ServeMux 路由语法，没法退回去迁就。
# 因此 windows/* 这三个产物实际可用范围是 Windows 10/11。
TARGETS := \
	linux/amd64 \
	linux/arm64 \
	linux/arm/7 \
	linux/arm/6 \
	linux/386 \
	windows/amd64 \
	windows/arm64 \
	windows/386

.DEFAULT_GOAL := help

.PHONY: help
help: ## 显示所有可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 构建当前平台的二进制
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: build-arm64
build-arm64: ## 构建树莓派用的 linux/arm64 二进制
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
	  go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

# outname 把 GOOS/GOARCH[/GOARM] 映射成产物文件名：
#   linux/arm/7     -> vidlink-linux-armv7
#   windows/amd64   -> vidlink-windows-amd64.exe
#
# Windows 的 .exe 必须自己加：`go build -o dist/x` 在 GOOS=windows 下
# **不会**补后缀（实测产物就叫 dist/vidlink-windows-amd64），
# 少了它用户在 Windows 上双击没反应、下载页也认不出是程序。
outname = $(BINARY)-$(subst /,-,$(subst arm/7,armv7,$(subst arm/6,armv6,$(TARGET))))$(if $(filter windows/%,$(TARGET)),.exe)

.PHONY: build-one
build-one: ## 构建单个目标：make build-one TARGET=linux/arm64
	@test -n "$(TARGET)" || { echo "用法: make build-one TARGET=linux/arm64"; exit 1; }
	@mkdir -p dist
	@set -- $(subst /, ,$(TARGET)); \
	  os=$$1; arch=$$2; arm=$$3; \
	  echo "-> $(TARGET)"; \
	  GOARM=$$arm CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(outname) . && \
	  ls -lh dist/$(outname) | awk '{print "   " $$9 "  " $$5}'

.PHONY: build-all
build-all: ## 交叉编译全部目标到 dist/（一个失败就整体失败）
	@mkdir -p dist
	@fail=""; \
	for t in $(TARGETS); do \
	  $(MAKE) --no-print-directory build-one TARGET=$$t || fail="$$fail $$t"; \
	done; \
	echo; echo "产物清单:"; ls -1sh dist/ | tail -n +2; \
	if [ -n "$$fail" ]; then echo; echo "以下目标构建失败:$$fail"; exit 1; fi; \
	echo; echo "全部 $$(echo $(TARGETS) | wc -w) 个目标构建成功";

.PHONY: run
run: ## 本地启动（默认 :8080）
	go run . 

.PHONY: fmt
fmt: ## 格式化
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 用；有未格式化文件则失败）
	@out=$$(gofmt -l . | grep -v '^refs/' || true); \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: test
test: ## 单元测试（不含实网联调）
	go test ./...

.PHONY: test-live
test-live: ## 实网联调（需要 VIDLINK_LIVE=1 与真实 Cookie）
	VIDLINK_LIVE=1 go test ./... -run Live -v

.PHONY: cover
cover: ## 覆盖率报告
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

.PHONY: check
check: fmt-check vet test ## CI 入口：格式 + 静态检查 + 测试

.PHONY: mint-douyin
mint-douyin: ## 铸造一个抖音访客身份（需要 curl 与 node；脚本仅本地保留，未随仓库发布）
	@test -x ./tools/douyin-mint/mint-identity.sh || { \
	  echo "tools/douyin-mint/ 未随仓库发布，请使用你本地保留的副本"; exit 1; }
	@./tools/douyin-mint/mint-identity.sh

.PHONY: openapi
openapi: ## 校验 OpenAPI 规格可解析
	@python3 -c "import yaml,sys; yaml.safe_load(open('docs/openapi.yaml')); print('openapi.yaml OK')"

.PHONY: docker
docker: ## 构建容器镜像
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):latest .

.PHONY: size
size: build ## 报告二进制体积与依赖数
	@echo "二进制: $$(du -h $(BINARY) | cut -f1)"
	@echo "第三方依赖: $$(go list -m all | tail -n +2 | wc -l) 个"

.PHONY: clean
clean: ## 清理构建产物
	rm -rf $(BINARY) $(BINARY)-linux-arm64 dist coverage.out
