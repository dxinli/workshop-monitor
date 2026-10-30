.PHONY: help build verify test lint lint-install vet fmt fmt-check tidy-check build-static up down seed verify-config preflight logs clean check hooks

GO ?= go
BIN_DIR := bin
# lint 工具版本锁补丁号，与「版本一律锁补丁号」一致。
# 换版本要同步 .github/workflows/ci.yml 的 golangci-lint version 字段。
# 若首次运行报 "Go language version used to build golangci-lint is lower than
# the targeted Go version"，说明本机 go 比该版本新，降到 v2.4.0 再试。
LINT_VERSION := v2.15.0
LINT ?= golangci-lint
COMPOSE := docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env --env-file .env

help:
	@echo "build         构建二进制"
	@echo "check         提交前总检查：与 CI 快速检查同一集"
	@echo "hooks         安装 git 钩子（core.hooksPath）"
	@echo "verify        校验依赖版本与镜像标签一致"
	@echo "test          单元测试"
	@echo "vet           官方 go vet"
	@echo "lint          golangci-lint（.golangci.yml）"
	@echo "lint-install  安装 golangci-lint $(LINT_VERSION)"
	@echo "fmt           格式化"
	@echo "tidy-check    go mod tidy 后 go.mod/go.sum 须无变更"
	@echo "up            启动全栈"
	@echo "down          停止全栈"
	@echo "seed          从 config/ 生成种子数据并导入"
	@echo "verify-config 配置校验 V-01~V-12"
	@echo "preflight     部署后自检"
	@echo "logs          跟随容器日志"

# 与 .github/workflows/ci.yml 的 go + config + aux 三个 job 保持同一集。
# 两边任一增删检查项，必须同步改另一边。
check: verify tidy-check fmt-check vet lint test build-static
	@if command -v docker >/dev/null 2>&1; then \
		docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env config -q \
			&& echo "compose 语法检查通过"; \
	else echo "跳过 compose 语法检查（docker 不可用）"; fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck --severity=error scripts/*.sh scripts/hooks/* && echo "shellcheck 通过"; \
	else echo "跳过 shellcheck（未安装）"; fi
	@if command -v python >/dev/null 2>&1; then \
		python -m compileall -q sim && echo "仿真件语法通过"; \
	else echo "跳过仿真件语法检查（未安装 python）"; fi
	@echo "check 通过"

tidy-check:
	@$(GO) mod tidy
	@if [ -n "$$(git status --porcelain -- go.mod go.sum 2>/dev/null)" ]; then \
		echo "go.mod 或 go.sum 未同步，已执行 go mod tidy，请提交变更"; \
		git status --porcelain -- go.mod go.sum; exit 1; fi
	@echo "依赖文件一致"

build-static:
	@CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o /dev/null ./cmd/edge-agent
	@CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o /dev/null ./cmd/proto-adapter
	@echo "静态构建通过"

fmt-check:
	@out=$$(gofmt -l . 2>&1); \
	if [ -n "$$out" ]; then echo "以下文件未格式化，执行 make fmt："; echo "$$out"; exit 1; fi
	@echo "格式检查通过"

hooks:
	git config core.hooksPath scripts/hooks
	@chmod +x scripts/hooks/* 2>/dev/null || true
	@echo "钩子已安装，路径 scripts/hooks"

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN_DIR)/edge-agent ./cmd/edge-agent
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN_DIR)/proto-adapter ./cmd/proto-adapter

verify:
	@test -f deploy/versions.env || { echo "缺少 deploy/versions.env"; exit 1; }
	@if grep -rn "latest" deploy/ config/ 2>/dev/null; then echo "发现 latest 浮动标签"; exit 1; fi
	@echo "版本锁定检查通过"

test:
	$(GO) test ./...

# 官方静态检查。抓 printf 格式、struct tag、不可达代码等真 bug，不做风格。
vet:
	$(GO) vet ./...

# golangci-lint：配置见 .golangci.yml。
# 缺二进制时 fail-closed——没有 lint 就报「通过」比不 lint 更糟。
lint:
	@command -v $(LINT) >/dev/null 2>&1 || { \
		echo "未找到 $(LINT)。安装："; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)"; \
		echo "或设置 LINT=/path/to/golangci-lint"; \
		exit 1; }
	$(LINT) run ./...

# 一次性安装 lint 工具到 GOPATH/bin
lint-install:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)
	@echo "已安装 $(LINT_VERSION) 到 $$($(GO) env GOPATH)/bin"

fmt:
	$(GO) fmt ./...

up:
	@test -f .env || { echo "缺少 .env，先执行 cp .env.example .env"; exit 1; }
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

seed:
	@echo "TODO: 从 config/ 生成种子数据并导入"

verify-config:
	@echo "TODO: 配置校验 V-01~V-12"

preflight:
	COMPOSE="$(COMPOSE)" ./scripts/preflight.sh

logs:
	$(COMPOSE) logs -f

clean:
	rm -rf $(BIN_DIR)
