# Mecharion (m7n) —— 构建
#
# 无 make 时可直接用 go：
#   go build ./...            构建全部
#   go test ./...             测试
#   go run ./cmd/mechctl ...  运行

SHELL := /bin/sh

MODULE  := github.com/mecharion/mecharion
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VPKG    := $(MODULE)/internal/version
LDFLAGS := -s -w \
	-X $(VPKG).Version=$(VERSION) \
	-X $(VPKG).Commit=$(COMMIT) \
	-X $(VPKG).Date=$(DATE)

BIN := bin

# 静态分析工具版本固定，理由与 sqlc/proto 生成物一致性检查同一条：
# 不固定的话，CI 今天绿明天红不是因为代码变了，是工具自己变了。
STATICCHECK_VERSION := v0.7.0
GOVULNCHECK_VERSION := v1.6.0
GOSEC_VERSION        := v2.28.0
# 模块路径是 zricethezav（改组织名前的历史路径），不是仓库现在的
# github.com/gitleaks/gitleaks——两者都能在 GitHub 上找到这个项目，
# 但只有前者是真正的 go module path，用后者 go run 会报路径不匹配。
GITLEAKS_VERSION     := v8.30.1
# lychee 是 Rust 写的，没有 go run 这条路，本地跑 make lychee 需要
# 自己先装（cargo install lychee 或从 GitHub Releases 下二进制）；
# CI 里下载的是固定版本的预编译产物，见 .github/workflows/ci.yml。
LYCHEE_VERSION       := lychee-v0.24.2
# 发布流程用的三个工具，版本固定理由同上。三个都是 Go module，走
# go install——与 sqlc 同一个理由：不用额外的第三方 GitHub Action，
# 版本纪律集中在这一处，CI 与本地跑的是完全一样的二进制。
GORELEASER_VERSION   := v2.17.1
SYFT_VERSION         := v1.51.0
COSIGN_VERSION       := v3.1.3

# 四个二进制在不同平台上的可用性不同：
#   mechd / mechlet  依赖 systemd 等 Linux 设施 → 只发布 Linux
#   mechctl / mechpack 是运维与开发者工具       → 全平台
LINUX_ONLY := mechd mechlet
PORTABLE   := mechctl mechpack
ALL        := $(LINUX_ONLY) $(PORTABLE)

.DEFAULT_GOAL := build

.PHONY: build
build: ## 为当前平台构建全部二进制
	@mkdir -p $(BIN)
	@for b in $(ALL); do \
		echo "  build  $$b"; \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$$b ./cmd/$$b || exit 1; \
	done

.PHONY: test
test: ## 运行测试
	go test -race -cover ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: staticcheck
staticcheck: ## 深度静态分析（真实发现，阻断 CI）
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

.PHONY: govulncheck
govulncheck: ## 已知漏洞依赖扫描（阻断 CI）
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: gosec
gosec: ## 安全模式扫描——建议性，不阻断 CI（见 .github/workflows/ci.yml 的说明）
	go run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) ./... || true

.PHONY: gitleaks
gitleaks: ## secret 扫描，含 git 历史（阻断 CI）；已知误报见 .gitleaksignore
	go run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) detect --source . -v

.PHONY: lychee
lychee: ## Markdown 链接检查，含外部 URL（阻断 CI）；需要本地已装 lychee
	@command -v lychee >/dev/null || { echo "需要先装 lychee（cargo install lychee，或从 GitHub Releases 下载 $(LYCHEE_VERSION)）"; exit 1; }
	lychee --no-progress --config .lychee.toml "**/*.md"

.PHONY: fmt
fmt: ## 格式化
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 用）
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi

.PHONY: tidy-check
tidy-check: ## 检查 go.mod/go.sum 是否已 tidy（CI 用）
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak
	@go mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		echo "go.mod / go.sum 未 tidy，请运行 make tidy"; exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

# ── 代码生成 ────────────────────────────────────────────────────────────
# 生成物**提交进仓库**：离线环境下构建不能依赖再跑一次生成，
# CI 与普通开发者也就不需要装 sqlc。只有改了 SQL 的人才需要。

.PHONY: webui
webui: ## 构建 Web UI 并拷进 internal/webui/dist（需要 Node）
	go generate -tags generate ./internal/webui/...

.PHONY: webui-test
webui-test: ## 跑前端测试（需要 Node）
	cd webui && npm run test

.PHONY: sqlc
sqlc: ## 由 internal/store/queries 重新生成访问层（需先 make tools）
	cd internal/store && sqlc generate

.PHONY: sqlc-check
sqlc-check: ## 检查生成物与 SQL 是否一致（改了 SQL 却忘了重新生成）
	@cd internal/store && sqlc generate
# 与 proto-check 同一条理由：git diff --quiet 只看**已跟踪**的文件，
# 新生成的文件还没提交过时会静默通过——而那正是最该拦住的时候。
	@if [ -n "$$(git status --porcelain -- internal/store/sqlcgen)" ]; then \
		echo "internal/store/sqlcgen 与 queries/ 不一致——请运行 make sqlc 并提交"; \
		git status --short -- internal/store/sqlcgen; \
		exit 1; \
	fi

.PHONY: proto
proto: ## 由 proto/ 重新生成 gRPC 代码
	@go run ./hack/protogen

.PHONY: proto-check
proto-check: ## 检查生成物与 .proto 是否一致（改了 proto 却忘了重新生成）
	@go run ./hack/protogen >/dev/null
# 用 status --porcelain 而不是 diff --quiet：后者只看已跟踪的文件，
# 生成物还没提交过时会**静默通过**，那正是最该拦住的时候。
	@if [ -n "$$(git status --porcelain -- internal/protocol/agentpb)" ]; then \
		echo "internal/protocol/agentpb 与 proto/ 不一致——请运行 make proto 并提交"; \
		git status --short -- internal/protocol/agentpb; \
		exit 1; \
	fi

.PHONY: tools
tools: ## 安装开发期工具（仅改 SQL 时需要）
# 版本固定，理由与 STATICCHECK_VERSION 等同一条：不固定的话，
# sqlc-check 今天绿明天红不是因为 SQL 变了，是 sqlc 自己变了。
# internal/store/sqlcgen/*.go 头部的 "versions: sqlc vX.Y.Z" 就是
# 已提交生成物当初用的版本——这里必须跟它一致。
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1

# 三层是按"要不要装 Node""要不要联网装工具"分的，不是按"重不重要"分的
# （REV-032）：一个只改 Go 代码的人不该被逼着装 Node 或联网拉 staticcheck；
# 一个要推送前确认一切正常的人才需要 check-all。`check` 单独留着，指向
# 最快那层——历史上一直是这个意思，不改名字避免打断已有的肌肉记忆。

.PHONY: check
check: check-fast ## = check-fast（本地最快的一轮：fmt/vet/test，无需 Node 或联网）

.PHONY: check-fast
check-fast: fmt-check vet test ## 最快一轮：格式、vet、单测——不需要 Node，不需要联网装工具

.PHONY: check-web
check-web: ## Web UI 的 lint + 测试（需要 Node）
	cd webui && npm run lint && npm run test

.PHONY: check-all
check-all: check-fast check-web tidy-check proto-check tools sqlc-check staticcheck govulncheck gosec gitleaks lychee ## 推送前的完整一轮：以上全部 + 静态分析/secret/链接检查（需要 Node、联网、本地装好 lychee）

.PHONY: release-tools
release-tools: ## 安装发布流程用到的三个工具（仅本地跑 release-dry-run 时需要）
	go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	go install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)
	go install github.com/sigstore/cosign/v3/cmd/cosign@$(COSIGN_VERSION)

.PHONY: release-check
release-check: ## 校验 .goreleaser.yaml 本身的语法/schema（快，不构建）
	goreleaser check

.PHONY: release-dry-run
release-dry-run: ## 本地跑一遍完整发布流程（构建+打包+SBOM，不签名/不发布，不需要 tag 或 token）
# 不签名：cosign keyless 要靠 GitHub Actions 的 OIDC token 换证书，本地
# 环境没有这个身份，`--skip=publish` 之外还要跳过 sign，否则本地会
# 卡在等浏览器交互式登录 Sigstore——那不是这个目标要验证的东西。
	goreleaser release --snapshot --clean --skip=publish --skip=sign

.PHONY: dist
dist: ## 交叉编译发布产物到 dist/
	@rm -rf dist && mkdir -p dist
	@set -e; \
	for p in linux/amd64 linux/arm64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		for b in $(ALL); do \
			echo "  dist   $$b $$os/$$arch"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
				go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$b-$$os-$$arch ./cmd/$$b; \
		done; \
	done; \
	for p in darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		if [ "$$os" = windows ]; then ext=".exe"; fi; \
		for b in $(PORTABLE); do \
			echo "  dist   $$b $$os/$$arch"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
				go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$b-$$os-$$arch$$ext ./cmd/$$b; \
		done; \
	done

# ── 测试环境 ────────────────────────────────────────────────────────────
# 集成测试需要一台能跑 systemd 的 Linux。测试镜像刻意"贫瘠"（无 curl/wget/
# 包管理器索引），使 hermetic 违规**直接失败**而非意外成功。
#
# 在 Windows 上开发时：用 `make testbin` 交叉编译，再从 WSL2 运行 testenv.sh。

.PHONY: testbin
testbin: ## 为测试容器交叉编译 linux/amd64 二进制到 bin/
	@mkdir -p $(BIN)
	@for b in $(ALL); do \
		echo "  testbin  $$b"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$$b ./cmd/$$b || exit 1; \
	done

.PHONY: testenv-up
testenv-up: testbin ## 启动 systemd 测试容器
	./hack/testenv.sh up

.PHONY: e2ebin
e2ebin: ## 交叉编译端到端测试所需的一切到 bin/（只需 Go）
	./hack/e2ebin.sh

.PHONY: e2e
e2e: e2ebin ## 在 systemd 容器里跑全部需要真机的测试（含 M2 验收）
	./hack/testenv.sh up
	./hack/testenv.sh test

# Windows + WSL2 上 Go 与 docker 常常不在同一侧，`make e2e` 跑不通。
# 那时分两步：
#   宿主机（有 Go）:  ./hack/e2ebin.sh
#   WSL（有 docker）: ./hack/testenv.sh up && ./hack/testenv.sh test

.PHONY: e2e-docker
e2e-docker: e2ebin ## 在**带 dockerd 的**容器里跑测试（M4 的 docker / compose runtime）
	./hack/testenv.sh --docker up
	./hack/testenv.sh --docker test

.PHONY: e2e-cluster
e2e-cluster: e2ebin ## 在**三节点集群**里跑多节点验收（M7）
	./hack/testenv.sh cluster up
	./hack/testenv.sh cluster test

.PHONY: testenv-cluster-up
testenv-cluster-up: testbin ## 启动三节点集群
	./hack/testenv.sh cluster up

.PHONY: testenv-docker-up
testenv-docker-up: testbin ## 启动带 dockerd 的测试容器
	./hack/testenv.sh --docker up

.PHONY: testenv-down
testenv-down: ## 停止并删除测试容器
	./hack/testenv.sh down
	./hack/testenv.sh --docker down

.PHONY: testenv-shell
testenv-shell: ## 进入测试容器
	./hack/testenv.sh shell

.PHONY: clean
clean: ## 清理产物
	rm -rf $(BIN) dist

.PHONY: help
help: ## 列出可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
