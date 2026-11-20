# Go is not required on the host: every target runs the toolchain in Docker.
GO_IMAGE := golang:1.27-alpine
CACHE    := $(HOME)/.cache/readroom-go
GO       := docker run --rm --user $(shell id -u):$(shell id -g) \
            -e HOME=/cache -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 \
            -v $(CACHE):/cache -v $(CURDIR):/src -w /src $(GO_IMAGE)

VERSION  := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: build install test vet tidy run image image-multiarch image-qnap

build: ## static binary at bin/readroom (runs on the host too)
	@mkdir -p $(CACHE) bin
	$(GO) go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/readroom ./cmd/readroom

install: build ## the binary of this checkout and the bundled text-to-speech script into ~/.local/bin; restarts the worker if it runs
	install -D bin/readroom $(HOME)/.local/bin/readroom
	install -D deploy/tts/tiny-voca-speak $(HOME)/.local/bin/tiny-voca-speak
	@if systemctl --user --quiet is-active readroom-worker.service 2>/dev/null; then \
		systemctl --user restart readroom-worker.service && echo 'installed in ~/.local/bin; restarted the worker'; \
	else \
		echo 'installed in ~/.local/bin; to set up the worker: readroom worker init'; \
	fi

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

image-qnap: ## arm64 archive in the classic "docker save" layout, which Container Station's importer requires
	@mkdir -p dist
	docker buildx build --platform linux/arm64 -t readroom:latest -o type=docker,dest=dist/readroom-arm64-oci.tar .
	@rm -f dist/readroom-arm64.tar
	docker run --rm -v $(CURDIR)/dist:/d quay.io/skopeo/stable:latest copy --override-arch arm64 --override-os linux \
		docker-archive:/d/readroom-arm64-oci.tar docker-archive:/d/readroom-arm64.tar:readroom:latest
	@rm -f dist/readroom-arm64-oci.tar
