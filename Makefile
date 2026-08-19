VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/noir017/agent-tools-mcp/internal/app.Version=$(VERSION)
BIN := bin/agent-tools-mcp

.PHONY: all build test lint fmt vet run docker clean check

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agent-tools-mcp

test:
	go test ./... -count=1

# 策略规则改动后跑一遍：里面既有"必须放行"也有"必须拦下"的用例
test-policy:
	go test ./internal/policy/ -v -count=1

fmt:
	gofmt -l -w .

vet:
	go vet ./...

check: fmt vet test

run: build
	./$(BIN) serve -c configs/config.yaml

docker:
	docker build -f deploy/Dockerfile -t agent-tools-mcp:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin
