.PHONY: help build verify test lint lint-install vet fmt fmt-check tidy-check build-static up down seed seed-sql verify-config config-check preflight logs clean check hooks passwd verify-broker verify-notify sim-pty sim-test sim-up

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
	@echo "seed          导入 02_seed.sql（先 make seed-sql 生成）"
	@echo "seed-sql      从 config/ 生成 02_seed.sql，不导入"
	@echo "config-check  提交前：配置合法且 02_seed.sql 与配置同步"
	@echo "verify-config 配置校验 V-01~V-11（V-12 由运行时记录）"
	@echo "preflight     部署后自检"
	@echo "passwd        从 .env 生成 Mosquitto 口令文件"
	@echo "verify-broker 003 验收：认证、授权、LWT、两套命名方案"
	@echo "verify-notify 011 验收：卡片字段、级别分流、重试与投递日志"
	@echo "logs          跟随容器日志"
	@echo "sim-pty       建 10 对虚拟串口，端点表写到 .pty/"
	@echo "sim-test      仿真件单元测试（需 sim/modbus-slave-sim 的 venv）"
	@echo "sim-up        起 Modbus 从站模拟器（需先 sim-pty）"

# 与 .github/workflows/ci.yml 的 go + config + aux 三个 job 保持同一集。
# 两边任一增删检查项，必须同步改另一边。
#
# compose 语法检查补的是 .env.example，不是 .env：.env 不进版本库，CI 里没有。
# compose 遇到未定义变量只告警不报错，于是「变量名写错」在本地和 CI 都不会暴露。
# 这里把该告警升为失败：compose 引用的每个变量都必须在 .env.example 里有同名键。
check: verify tidy-check fmt-check vet lint test build-static config-check
	@if command -v docker >/dev/null 2>&1; then \
		out=$$(docker compose -f deploy/docker-compose.yml \
			--env-file deploy/versions.env --env-file .env.example config -q 2>&1); \
		if printf '%s' "$$out" | grep -q 'is not set'; then \
			echo "compose 引用了 .env.example 里没有的变量："; \
			printf '%s\n' "$$out" | grep 'is not set'; \
			exit 1; \
		fi; \
		if [ -n "$$out" ]; then echo "$$out"; exit 1; fi; \
		echo "compose 语法检查通过"; \
	else echo "跳过 compose 语法检查（docker 不可用）"; fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck --severity=error scripts/*.sh .githooks/* && echo "shellcheck 通过"; \
	else echo "跳过 shellcheck（未安装）"; fi
	@if command -v python >/dev/null 2>&1; then \
		python -m compileall -q sim && echo "仿真件语法通过"; \
	else echo "跳过仿真件语法检查（未安装 python）"; fi
	@if [ -x "$(SIM_PY)" ]; then \
		$(SIM_PY) -m pytest sim/modbus-slave-sim -q || exit 1; \
	else echo "跳过仿真件单元测试（无 .venv-sim）"; fi
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

# 导入 02_seed.sql。重复执行结果相同：脚本先清空配置域再重建。
seed: bin/config-tool
	@test -f deploy/timescaledb/init/02_seed.sql || { \
		echo "先生成种子脚本：make seed-sql"; exit 1; }
	$(COMPOSE) exec -T timescaledb sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -v ON_ERROR_STOP=1 -f -' \
		< deploy/timescaledb/init/02_seed.sql

# 从 config/ 生成 02_seed.sql，不导入。配置改动后跑这个，把生成物一并提交。
seed-sql: bin/config-tool
	@bin/config-tool seed

verify-config: bin/config-tool
	@./scripts/verify-config.sh bin/config-tool

# 提交前检查：配置合法，且 02_seed.sql 与配置文件同步。
# 种子是生成物却要进仓库——tag 与 git diff 是唯一能证明「库里投影来自当前配置」的手段。
config-check: bin/config-tool
	@bin/config-tool verify >/dev/null
	@tmp=$$(mktemp); \
	 bin/config-tool seed -out "$$tmp" >/dev/null || { rm -f "$$tmp"; exit 1; }; \
	 if ! diff -q "$$tmp" deploy/timescaledb/init/02_seed.sql >/dev/null; then \
		echo "02_seed.sql 与 config/ 不一致，先执行 make seed-sql"; \
		diff "$$tmp" deploy/timescaledb/init/02_seed.sql | head -20; \
		rm -f "$$tmp"; exit 1; \
	 fi; \
	 rm -f "$$tmp"; echo "配置与种子脚本一致"

# 配置校验与种子生成共用同一份二进制，两个目标都依赖它。
bin/config-tool: $(shell find internal/config cmd/config-tool -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $@ ./cmd/config-tool

preflight:
	COMPOSE="$(COMPOSE)" ./scripts/preflight.sh

passwd:
	./scripts/gen-passwd.sh

verify-broker:
	./scripts/verify-broker.sh

# 需要已 seed 的库与已构建的 node-red 镜像；NOTIFY_ENV_FILE 可指向别的 .env。
verify-notify:
	./scripts/verify-notify.sh

logs:
	$(COMPOSE) logs -f

# 仿真件的 Python 依赖装在 .venv-sim（与 CI 同名），不进交付镜像。
# 缺 venv 时 fail-closed：不跑测试却报「通过」比不跑更糟。
SIM_PY := .venv-sim/bin/python

sim-pty:
	./scripts/setup-pty.sh

sim-test:
	@test -x "$(SIM_PY)" || { \
		echo "缺少 $(SIM_PY)，先执行："; \
		echo "  python3.12 -m venv .venv-sim"; \
		echo "  .venv-sim/bin/pip install -r sim/modbus-slave-sim/requirements.txt pytest"; \
		exit 1; }
	$(SIM_PY) -m pytest sim/modbus-slave-sim -q

sim-up:
	@test -f .pty/ports.json || { echo "缺少 .pty/ports.json，先执行 make sim-pty"; exit 1; }
	@test -x "$(SIM_PY)" || { echo "缺少 $(SIM_PY)，见 make sim-test 的提示"; exit 1; }
	$(SIM_PY) sim/modbus-slave-sim/main.py --config config/terminals --ports .pty/ports.json

clean:
	rm -rf $(BIN_DIR)
