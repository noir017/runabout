VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/noir017/runabout/internal/app.Version=$(VERSION)
BIN := bin/runabout

.PHONY: all build build-amd64 build-arm64 test lint fmt vet run docker docker-gateway clean check

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/runabout

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 ./cmd/runabout

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-arm64 ./cmd/runabout

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
	docker build -f deploy/Dockerfile -t runabout:$(VERSION) --build-arg VERSION=$(VERSION) .

docker-gateway:
	docker build -f deploy/Dockerfile --target gateway -t runabout:$(VERSION)-gateway --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin
