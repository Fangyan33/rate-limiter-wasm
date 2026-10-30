SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:

VERSION ?= $(shell bash scripts/git-version.sh)
VERSION := $(VERSION)
ARCH ?= amd64
IMAGE_REPOSITORY ?= rate-limiter-counter-service
NAMESPACE ?= metering
IMAGE := $(IMAGE_REPOSITORY):$(VERSION)
DIST_DIR ?= dist
OUTPUT_DIR := $(abspath $(DIST_DIR))/$(VERSION)/linux-$(ARCH)
ARCHIVE := $(abspath $(DIST_DIR))/rate-limiter-wasm-$(VERSION)-linux-$(ARCH).tar.gz

.PHONY: all build wasm counter-service image artifacts version help check-version check-image check-output manifests

all: artifacts

check-version:
	@[[ "$(VERSION)" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$$ ]] || { echo '版本号必须是有效的 Docker tag，可通过 VERSION 显式指定。' >&2; exit 1; }

version: check-version
	@printf '%s\n' '$(VERSION)'

build: wasm counter-service

# 不清理或混入历史名称制品；旧目录保留，由调用方选择新的输出根目录。
check-output:
	@for old in counter-service counter-service-image.tar rate-limiter.wasm token-stats.wasm deploy/istio/counter-service-deployment.yaml; do \
		[[ ! -e "$(OUTPUT_DIR)/$$old" ]] || { echo '输出目录含旧制品，请通过 DIST_DIR 指定新目录；旧制品未修改。' >&2; exit 1; }; \
	done

wasm: check-version check-output
	bash ./build.sh "$(OUTPUT_DIR)"

counter-service: check-version check-output
	mkdir -p "$(OUTPUT_DIR)"
	CGO_ENABLED=0 GOOS=linux GOARCH="$(ARCH)" go build -buildvcs=false -o "$(OUTPUT_DIR)/rate-limiter-counter-service" ./rate-limiter/cmd/counter-service

# 从同一批文件组装镜像，保证独立 WASM 与镜像内下载的 WASM 一致。
check-image:
	@repository='$(IMAGE_REPOSITORY)'; [[ "$${repository##*/}" == rate-limiter-?* ]] || { echo '自有镜像的最后一级名称必须以 rate-limiter- 开头。' >&2; exit 1; }

image: check-image build
	docker build --platform "linux/$(ARCH)" \
		--label "org.opencontainers.image.version=$(VERSION)" \
		-t "$(IMAGE)" -f "$(CURDIR)/rate-limiter/Dockerfile.counter-service-artifacts" "$(OUTPUT_DIR)"

manifests: check-image wasm
	go run -buildvcs=false ./rate-limiter/cmd/render-manifests --output "$(OUTPUT_DIR)" --image "$(IMAGE)" --namespace "$(NAMESPACE)"

artifacts: image manifests
	docker save -o "$(OUTPUT_DIR)/rate-limiter-counter-service-image.tar" "$(IMAGE)"
	cp -R docs "$(OUTPUT_DIR)/"
	mkdir -p "$(OUTPUT_DIR)/scripts"
	sed 's|http://counter-service:8080|http://rate-limiter-counter-service:8080|g' scripts/import-service-configs.sh > "$(OUTPUT_DIR)/scripts/rate-limiter-import-service-configs.sh"
	chmod +x "$(OUTPUT_DIR)/scripts/rate-limiter-import-service-configs.sh"
	printf '%s\n' '$(VERSION)' > "$(OUTPUT_DIR)/VERSION"
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
		'make manifests         生成规范命名的部署清单及 simple 示例（默认 metering）' \
		'make version           输出自动选择的版本号' \
		'可选变量：VERSION、ARCH（默认 amd64）、IMAGE_REPOSITORY（末级名称以 rate-limiter- 开头）、NAMESPACE（默认 metering）、DIST_DIR'
