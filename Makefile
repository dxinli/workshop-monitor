.PHONY: help build verify test lint lint-install vet fmt fmt-check tidy-check build-static up down seed verify-config preflight logs clean check hooks passwd verify-broker

GO ?= go
BIN_DIR := bin
# lint 工具版本锁补丁号，与「版本一律锁补丁号」一致。
# 换版本要同步 .github/workflows/ci.yml 的 golangci-lint version 字段。
LINT_VERSION := v2.15.0
# golangci-lint 的安装落点，按 go 自己的规则推：
#   GOBIN 已设 → 直接用它（它本身就是放二进制的目录，不再拼 /bin）
#   GOBIN 空   → $(go env GOPATH)/bin
#   GOBIN、GOPATH 都取不到（没装 go）→ $(HOME)/go/bin
# 注意 GOPATH/bin 默认不在 PATH 上——这是「明明装了却报未找到」的第一原因。
GOPATH_DIR   := $(or $(shell $(GO) env GOPATH 2>/dev/null),$(HOME)/go)
LINT_BIN_DIR := $(or $(shell $(GO) env GOBIN 2>/dev/null),$(GOPATH_DIR)/bin)

# 位置解析：PATH 里优先，其次安装落点。两者都没有时落在落点上，由 lint 目标报错。
# 命令行或环境变量覆盖优先：make lint LINT=/usr/local/bin/golangci-lint
LINT := $(or $(shell command -v golangci-lint 2>/dev/null),$(LINT_BIN_DIR)/golangci-lint)

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
	@echo "passwd        从 .env 生成 Mosquitto 口令文件"
	@echo "verify-broker 003 验收：认证、授权、LWT、两套命名方案"
	@echo "logs          跟随容器日志"

# 与 .github/workflows/ci.yml 的 go + config + aux 三个 job 保持同一集。
# 两边任一增删检查项，必须同步改另一边。
check: verify tidy-check fmt-check vet lint test build-static
	@if command -v docker >/dev/null 2>&1; then \
		docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env config -q \
			&& echo "compose 语法检查通过"; \
	else echo "跳过 compose 语法检查（docker 不可用）"; fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck --severity=error scripts/*.sh .githooks/* && echo "shellcheck 通过"; \
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
	git config core.hooksPath .githooks
	@chmod +x .githooks/* 2>/dev/null || true
	@echo "钩子已安装，路径 .githooks"

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
	@test -x "$(LINT)" || { \
		echo "未找到可执行的 golangci-lint：$(LINT)"; \
		echo ""; \
		echo "本机 go                : $$($(GO) version 2>/dev/null || echo 未安装)"; \
		echo "PATH 中的 golangci-lint: $$(command -v golangci-lint 2>/dev/null || echo 没有)"; \
		echo "安装目录               : $(LINT_BIN_DIR)"; \
		echo "该目录在 PATH 上       : $$(printf '%s\n' "$$PATH" | tr ':' '\n' | grep -qxF '$(LINT_BIN_DIR)' && echo 是 || echo 否)"; \
		echo ""; \
		echo "装法一（推荐，不依赖本机 Go 版本）："; \
		echo "  make lint-install"; \
		echo "装法二（需 Go >= 1.26 工具链，内网或 GOTOOLCHAIN=local 会失败）："; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)"; \
		echo "已经装在别处："; \
		echo "  make lint LINT=/path/to/golangci-lint"; \
		exit 1; }
	@echo "lint 版本：$$($(LINT) --version 2>&1 | head -n1)"
	$(LINT) run ./...

lint-install:
	@command -v curl >/dev/null 2>&1 || { echo "需要 curl 下载预编译包"; exit 1; }
	@mkdir -p "$(LINT_BIN_DIR)"
	@echo "下载 golangci-lint $(LINT_VERSION) → $(LINT_BIN_DIR)"
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b "$(LINT_BIN_DIR)" $(LINT_VERSION)
	@if [ -x "$(LINT_BIN_DIR)/golangci-lint" ]; then \
		echo "已安装：$(LINT_BIN_DIR)/golangci-lint"; \
		"$(LINT_BIN_DIR)/golangci-lint" --version 2>&1 | head -n1; \
	else \
		echo "安装后仍找不到 $(LINT_BIN_DIR)/golangci-lint，检查该目录写权限"; exit 1; \
	fi

fmt:
	$(GO) fmt ./...

# 口令文件不入版本库，每次起栈前按 .env 重建，避免新克隆缺文件导致 broker 起不来。
up: passwd
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

passwd:
	./scripts/gen-passwd.sh

verify-broker:
	./scripts/verify-broker.sh

logs:
	$(COMPOSE) logs -f

clean:
	rm -rf $(BIN_DIR)
