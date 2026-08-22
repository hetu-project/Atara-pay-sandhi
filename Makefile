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

smoke: build    ## 端到端跑一遍两条主流程
	./scripts/smoke.sh
