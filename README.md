# rate-limiter-wasm

本仓库已按双插件架构拆分为三个 Go module：

- `shared/`：共享的 Bearer 解析、JWT `uid` 提取、域名匹配
- `rate-limiter/`：实际并发数限流插件与 `counter-service`
- `token-stats/`：LLM token usage 统计插件

## 目录

```text
rate-limiter-wasm/
├── shared/
├── rate-limiter/
├── token-stats/
├── deploy/istio/
└── docs/specs/2026-04-10-wasm-plugin-split-spec.md
```

## 构建

构建两个 WASM：

```bash
bash ./build.sh
```

产物输出到根目录 `dist/`：

- `dist/rate-limiter.wasm`
- `dist/token-stats.wasm`

构建 `counter-service`：

```bash
bash ./build-counter-service.sh
```

默认输出：

- `dist/counter-service`

## 测试

受工作区沙箱限制，运行 Go 测试时建议显式设置可写 `GOCACHE`：

```bash
GOCACHE=/tmp/go-build go test ./shared/...
GOCACHE=/tmp/go-build go test ./rate-limiter/internal/config ./rate-limiter/internal/limiter ./rate-limiter/internal/store ./rate-limiter/internal/plugin
GOCACHE=/tmp/go-build go test ./token-stats/internal/config ./token-stats/internal/plugin
```

`counter-service` 相关测试依赖 `miniredis` 监听本地端口；在当前受限沙箱中会失败，需要在允许本地监听的环境执行：

```bash
GOCACHE=/tmp/go-build go test ./rate-limiter/internal/counter-service/...
```

## 部署

双插件需独立部署为两个 EnvoyFilter：

- `deploy/istio/rate-limiter-envoyfilter.yaml`
- `deploy/istio/token-stats-envoyfilter.yaml`

对应独立配置：

- `deploy/istio/rate-limiter-plugin-config.yaml`
- `deploy/istio/token-stats-plugin-config.yaml`

注意事项：

- 两个插件都维护自己的 `domains`，运维侧必须保持一致。
- `rate-limiter` 继续依赖 `counter-service` 做分布式 acquire/release。
- `token-stats` 不依赖 `counter-service`，示例里使用独立的 `wasm-artifact-server` 拉取 `token-stats.wasm`。
- 部署前需要把 YAML 中的 `sha256` 替换为实际构建产物摘要。
