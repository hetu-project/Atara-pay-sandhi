.PHONY: run build test fmt vet clean smoke

run:            ## 起服务（自动建库 + 灌种子数据）
	go run ./cmd/atara-pay

build:
	go build -o bin/atara-pay ./cmd/atara-pay

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

clean:          ## 删库重来
	rm -f atara.db atara.db-wal atara.db-shm
	rm -rf var/uploads bin

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

chain-deploy:  ## 部署托管合约与两个测试稳定币，输出填进 .env
	@cd contracts && PRIVATE_KEY=$${ATARA_SIGNER_KEY:?set ATARA_SIGNER_KEY} \
	  forge script script/Deploy.s.sol --rpc-url $${ATARA_RPC_URL:-http://127.0.0.1:8545} \
	  --broadcast 2>&1 | grep -E '^  ATARA_' | sed 's/^  //'

chain-e2e:  ## 真链端到端：钱进合约、签证明、合约验签放款
	@python3 scripts/chain-e2e.py
