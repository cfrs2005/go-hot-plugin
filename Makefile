# go-hot-plugin：Go 热插拔插件 demo。只用本地 Go 工具链（GOTOOLCHAIN=local），不自动下载新版本。
export GOTOOLCHAIN := local
GO      ?= go
BIN     := bin
PLUGINS := vendor-a-v1 vendor-a-v2 vendor-b

.PHONY: build test verify run-vendorsim run-platform run-demo run-all clean

build:
	$(GO) build -o $(BIN)/platform  ./cmd/platform
	$(GO) build -o $(BIN)/demo-go   ./cmd/demo-go
	$(GO) build -o $(BIN)/vendorsim ./cmd/vendorsim
	$(GO) build -o $(BIN)/verify    ./cmd/verify
	@for p in $(PLUGINS); do $(GO) build -o $(BIN)/plugins/$$p ./plugins/$$p || exit 1; done

test:
	$(GO) vet ./...
	$(GO) test -race ./...

# 构建 → vet + 单测 → 端到端验收（起真进程，结束清理 var/verify）
verify: build test
	./$(BIN)/verify

# 三个服务各自前台运行（各开一个终端）
run-vendorsim: build
	./$(BIN)/vendorsim
run-platform: build
	./$(BIN)/platform
run-demo: build
	./$(BIN)/demo-go

# 一个终端里同时起三个服务，Ctrl-C 一起停
run-all: build
	@trap 'kill 0' INT TERM; \
	./$(BIN)/vendorsim & ./$(BIN)/platform & sleep 0.5; ./$(BIN)/demo-go & wait

clean:
	rm -rf $(BIN) var
