.PHONY: run run-chain fresh fresh-chain build test fmt vet clean smoke

# 库和上传目录在这里写一次，run / clean / fresh 都用它。
#
# 之前 run 走的是环境变量里的 /tmp/atara-dev.db，clean 删的却是仓库里的
# atara.db——两边各说各的。结果是「删完重启还是老数据」，而且看不出哪儿错了：
# 两条命令都成功返回。所以路径只能有一份。
DB      ?= /tmp/atara-dev.db
UPLOADS ?= ./var/uploads
ADDR    ?= 127.0.0.1:8080
WEB     ?= http://localhost:5173
PORT     = $(lastword $(subst :, ,$(ADDR)))

ENV = ATARA_DB_PATH=$(DB) ATARA_UPLOAD_DIR=$(UPLOADS) \
      ATARA_HTTP_ADDR=$(ADDR) ATARA_CORS_ORIGINS=$(WEB)

run:            ## 起服务（保留现有数据，mock 链）
	$(ENV) go run ./cmd/atara-pay

# .env 只有这两个目标会读。run / fresh 不读是故意的：那两个是演示用的
# mock 链，读了 .env 就会连真链，RPC 一不通后端直接起不来——
# 而人只是想开个演示。要连链就明说，走这两个目标。
run-chain:      ## 起服务，连 .env 里配的真链（保留现有数据）
	set -a && . ./.env && set +a && $(ENV) go run ./cmd/atara-pay

fresh-chain: clean  ## 删数据 + 连真链起
	set -a && . ./.env && set +a && $(ENV) go run ./cmd/atara-pay

fresh: clean    ## 删掉全部历史数据再起——每次从零开始
	@echo "== 空库启动：16 个种子用户 + 10 条挂单，没有任何 KYC 记录 =="
	$(ENV) go run ./cmd/atara-pay

build:
	go build -o bin/atara-pay ./cmd/atara-pay

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

# 先停再删。顺序反过来是没用的：SQLite 那个进程还开着文件句柄，
# 删掉的只是目录项，它照样读写那个 inode，重启前你看到的还是老数据。
# 用端口找进程而不是 pkill -f atara-pay——后者会连自己这行 sh 一起杀。
clean:          ## 删掉库、上传文件和编译产物
	@pids=$$(lsof -ti tcp:$(PORT) -sTCP:LISTEN 2>/dev/null); \
	 if [ -n "$$pids" ]; then \
	   for p in $$pids; do \
	     pp=$$(ps -o ppid= -p $$p 2>/dev/null | tr -d ' '); \
	     kill $$p 2>/dev/null || true; \
	     case "$$(ps -o command= -p $$pp 2>/dev/null)" in \
	       *"go run"*) kill $$pp 2>/dev/null || true;; \
	     esac; \
	   done; \
	   echo "   停掉 $(PORT) 上的旧进程：$$pids"; sleep 1; \
	 fi
	rm -f $(DB) $(DB)-wal $(DB)-shm
	rm -rf $(UPLOADS) bin
	@echo "   删掉 $(DB) 与 $(UPLOADS)"

smoke: build    ## 端到端跑一遍两条主流程与非托管的每一处分叉
	@rm -f /tmp/atara-smoke.db
	@ATARA_HTTP_ADDR=:8099 ATARA_DB_PATH=/tmp/atara-smoke.db ./bin/atara-pay > /tmp/atara-smoke.log 2>&1 & \
	 sleep 3; python3 scripts/smoke.py 8099; rc=$$?; \
	 pkill -f 'bin/atara-pay' 2>/dev/null; rm -f /tmp/atara-smoke.db*; exit $$rc

.PHONY: chain-up chain-deploy chain-e2e
chain-up:  ## 起本地测试链（chainId 97，与 BSC 测试网一致）
	@pkill -f anvil 2>/dev/null || true
	@anvil --chain-id 97 --silent > /tmp/anvil.log 2>&1 &
	@sleep 2 && echo "anvil on :8545"

# 用 Hardhat + viem，不需要 foundry。参数全从 .env 读。
#
#   make chain-deploy              演练：只检查，一笔交易都不发
#   make chain-deploy A="send"     真的部署
#   make chain-deploy A="send write"  部署完把地址写回 .env
#
# 默认演练是故意的：这个目标会往链上发不可撤销的交易、花掉真的 gas。
chain-deploy:  ## 部署托管合约（默认演练，A="send" 才真发）
	@cd contracts && [ -d node_modules ] || npm install
	@cd contracts && npx hardhat compile && npx tsx scripts/deploy.ts $(A)

chain-e2e:  ## 真链端到端：钱进合约、签证明、合约验签放款
	@python3 scripts/chain-e2e.py

chain-allowance:  ## 真链端到端：额度上链，链上撤销后平台立刻拒绝
	@python3 scripts/allowance-e2e.py
