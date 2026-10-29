SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:

VERSION ?= $(shell bash scripts/git-version.sh)
VERSION := $(VERSION)
ARCH ?= amd64
IMAGE_REPOSITORY ?= counter-service
IMAGE := $(IMAGE_REPOSITORY):$(VERSION)
DIST_DIR ?= dist
OUTPUT_DIR := $(abspath $(DIST_DIR))/$(VERSION)/linux-$(ARCH)
ARCHIVE := $(abspath $(DIST_DIR))/rate-limiter-wasm-$(VERSION)-linux-$(ARCH).tar.gz

.PHONY: all build wasm counter-service image artifacts version help check-version

all: artifacts

check-version:
	@[[ "$(VERSION)" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$$ ]] || { echo '版本号必须是有效的 Docker tag，可通过 VERSION 显式指定。' >&2; exit 1; }

version: check-version
	@printf '%s\n' '$(VERSION)'

build: wasm counter-service

wasm: check-version
	bash ./build.sh "$(OUTPUT_DIR)"

counter-service: check-version
	mkdir -p "$(OUTPUT_DIR)"
	CGO_ENABLED=0 GOOS=linux GOARCH="$(ARCH)" go build -buildvcs=false -o "$(OUTPUT_DIR)/counter-service" ./rate-limiter/cmd/counter-service

# 从同一批文件组装镜像，保证独立 WASM 与镜像内下载的 WASM 一致。
image: build
	docker build --platform "linux/$(ARCH)" \
		--label "org.opencontainers.image.version=$(VERSION)" \
		-t "$(IMAGE)" -f "$(CURDIR)/rate-limiter/Dockerfile.counter-service-artifacts" "$(OUTPUT_DIR)"

artifacts: image
	docker save -o "$(OUTPUT_DIR)/counter-service-image.tar" "$(IMAGE)"
	mkdir -p "$(OUTPUT_DIR)/deploy"
	cp -R deploy/istio "$(OUTPUT_DIR)/deploy/"
	cp -R docs "$(OUTPUT_DIR)/"
	mkdir -p "$(OUTPUT_DIR)/scripts"
	cp scripts/import-service-configs.sh "$(OUTPUT_DIR)/scripts/"
	@for plugin in rate-limiter token-stats; do \
		digest=$$(sha256sum "$(OUTPUT_DIR)/$$plugin.wasm" | cut -d ' ' -f 1); \
		sed -i -E "s/^( *sha256:).*/\1 $$digest/" "$(OUTPUT_DIR)/deploy/istio/$$plugin-envoyfilter.yaml"; \
	done
	sed -i -E 's|^( *image:) counter-service:latest$$|\1 $(IMAGE)|' "$(OUTPUT_DIR)/deploy/istio/counter-service-deployment.yaml"
	printf '%s\n' '$(VERSION)' > "$(OUTPUT_DIR)/VERSION"
	cp README.md "$(OUTPUT_DIR)/README.md"
	cd "$(OUTPUT_DIR)" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
	tar -czf "$(ARCHIVE)" -C "$(OUTPUT_DIR)" .
	cd "$(dir $(ARCHIVE))" && sha256sum "$(notdir $(ARCHIVE))" > "$(notdir $(ARCHIVE)).sha256"
	@printf '制品目录：%s\n制品归档：%s\n镜像：%s\n' "$(OUTPUT_DIR)" "$(ARCHIVE)" "$(IMAGE)"

help:
	@printf '%s\n' \
		'make / make artifacts  构建全部制品，导出镜像并生成归档和校验和' \
		'make build             构建 Linux counter-service 二进制和两个 WASM' \
		'make wasm              仅构建两个 WASM' \
		'make counter-service   仅构建 Linux counter-service 二进制' \
		'make image             构建二进制、WASM 和 Counter Service 镜像' \
		'make version           输出自动选择的版本号' \
		'可选变量：VERSION、ARCH（默认 amd64）、IMAGE_REPOSITORY、DIST_DIR'
