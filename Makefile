# Go is not required on the host: every target runs the toolchain in Docker.
GO_IMAGE := golang:1.27-alpine
CACHE    := $(HOME)/.cache/readroom-go
GO       := docker run --rm --user $(shell id -u):$(shell id -g) \
            -e HOME=/cache -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 \
            -v $(CACHE):/cache -v $(CURDIR):/src -w /src $(GO_IMAGE)

.PHONY: build test vet tidy run image image-multiarch

build: ## static binary at bin/readroom (runs on the host too)
	@mkdir -p $(CACHE) bin
	$(GO) go build -trimpath -o bin/readroom ./cmd/readroom

test:
	@mkdir -p $(CACHE)
	$(GO) go test ./...

vet:
	$(GO) go vet ./...

tidy:
	$(GO) go mod tidy

run: build ## run locally on :8090 with ./readroom.db, no token
	READROOM_LOG=debug ./bin/readroom

image:
	docker build -t readroom:latest .

image-multiarch: ## amd64 + arm64, written to dist/ as OCI archives for loading on the NAS
	@mkdir -p dist
	docker buildx build --platform linux/amd64 -t readroom:latest -o type=docker,dest=dist/readroom-amd64.tar .
	docker buildx build --platform linux/arm64 -t readroom:latest -o type=docker,dest=dist/readroom-arm64.tar .
