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
