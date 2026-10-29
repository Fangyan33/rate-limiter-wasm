# 使用本地 Kubernetes 集群执行 Istio 端到端测试

Istio E2E 使用本地 Kubernetes 集群，当前为本机 kind 集群 `kind`，context 为 `kind-kind`。`scripts/k8s_e2e.py` 默认固定使用 `kind-kind`，所有 kubectl 操作显式携带 context；切换其他本地集群时使用 `--context` 指定。脚本启动时输出目标 context 与 API Server 地址，并将地址写入 `environment.json` 和报告。

脚本在本地集群中创建独立命名空间，复用现有 Istio 控制面，运行两个测试网关副本、当前限流 WASM 插件（使用 `--rate-limiter-only`）、Counter Service 和临时 Redis。HTTP/SSE 上游由测试 fixture 提供；透明代理把正常 acquire/release/renew 转发给真实 Counter Service，并为故障用例注入 500、503、非法 JSON 和超时。

依赖 Python 3 标准库、kubectl，以及已有的 Kubernetes/Istio 集群。本次执行环境为 Kubernetes 1.22.17、Istio 1.13.5。`token-stats` 已废弃、停止维护；限流专项不部署该插件，也不要求 `extraStatTags`。原完整入口仅为保留现状，已废弃的统计用例不属于本轮验收。

在仓库根目录构建并将镜像导入本地 kind 集群：

```bash
make image VERSION=e2e-local DIST_DIR=dist/e2e-build
kind load docker-image counter-service:e2e-local python:3.10-slim redis:8.6.1 --name kind

python3 -B scripts/k8s_e2e.py \
  --context kind-kind --rate-limiter-only \
  --image counter-service:e2e-local \
  --artifacts dist/e2e-build/e2e-local/linux-amd64 \
  --output dist/e2e/latest
```

`--image` 必须对应 `--artifacts` 中的 Wasm；原双插件构建及制品保留，限流专项只加载 rate-limiter。测试清单使用 `imagePullPolicy: Never`，需事先保证各节点存在所需镜像。其他本地集群需自行导入这些镜像。

脚本自动生成唯一命名空间，结束时删除测试资源并停止本机 port-forward。使用 `--keep` 可保留资源调试，实际命名空间记录在 `environment.json` 和报告中。脚本比较测试前后的原有 Deployment、EnvoyFilter 和 ConfigMap 配置，结果也进入报告。

限流专项覆盖配置 CRUD/分页、SHA256与写入hash校验、查询不含api_key、必填enabled、description清空、小写域名、key优先及禁用回退、无规则放行、并发限制、释放幂等、TTL、续约、跨副本长流、配置热更新、Redis配置损坏、acquire/renew/release故障放行和Redis实际停机。Redis 停机后数据会丢弃，这是测试 Redis 的预期行为。

限流插件使用 ECDS、内置 Lua 透传默认配置、`fail_open: true` 和 `nack_on_code_cache_miss: true`。保留的统计清单维持原加载方式。测试会先确认真实限流已经激活，再执行策略断言，避免把默认透传当成插件正常。加载故障测试覆盖不存在的下载 Cluster、连接拒绝、DNS 失败、摘要不匹配、404、下载超时、损坏模块、非法业务配置、缺少运行时，以及无下载源时的网关冷启动。配置切换期间持续对两个副本发送请求，记录任何非 200 或连接异常。

要进一步验证初始化失败和运行时 trap，构建仅供测试的故障模块，并传给脚本：

```bash
tinygo build -o dist/fault.wasm -scheduler=none -target=wasi scripts/testdata/fault-wasm/main.go
python3 -B scripts/k8s_e2e.py \
  --context kind-kind --rate-limiter-only \
  --image counter-service:e2e-local \
  --artifacts dist/e2e-build/e2e-local/linux-amd64 \
  --fault-wasm dist/fault.wasm \
  --output dist/e2e/fail-open
```

`--loading-only` 可仅运行加载故障测试。故障模块会被放进隔离命名空间的 ConfigMap；脚本使用服务端 apply，避免二进制内容重复写入 last-applied 注解超过大小限制。测试模块不得部署到业务网关。

故障断言还覆盖 acquire 空体、null、缺少 allowed、未知拒绝原因、旧 api_key_disabled/config_not_found、HTTP200内部错误、超大响应；release 503/非法体/超时；Redis停机时重启Counter Service仍能下载完整Wasm。释放失败后确认租约指针到期，再验证下一次acquire清理ZSET过期成员，不要求ZSET成员自行消失。

Istio 1.13.5 中，Agent 下载失败可能将原始远程 Wasm 配置交给 Envoy。缺少 `nack_on_code_cache_miss` 的 ECDS 配置曾在本地故障注入时触发 Envoy 1.21.3-dev 段错误。该保护让未缓存的扩展被拒绝、监听器继续使用透传默认配置；不能把原来内联加载的监听器和 ECDS 的失败策略混用。

退出码0表示本轮全部断言通过，1表示失败。输出包含report.md、results.json、实际manifest.json、故障响应、网关配置和日志。限流专项在Redis停机时验证直接acquire在800ms内得到内部503，网关确实收到该503并继续转发至模拟上游，返回业务200。

测试租约 TTL 为 1500ms，以便快速验证续约。初始化和恢复均先创建启用规则并通过真实租约占满额度，确认两个网关返回超限429，随后释放并删除探针；缺规则和禁用规则不再用于激活检测。这些结果覆盖当前集群中的功能行为；大规模压测、长时间稳定性和真实模型服务验收仍需单独执行。

## 2026-10-29 新规则专项验证

使用 `counter-service:rules-20261029-final` 在本地 kind-kind / Istio 1.13.5 的隔离双副本网关运行 `--rate-limiter-only` 与故障Wasm，56项全部通过。覆盖新优先级、禁用回退、无规则无租约放行、查询回写400、Redis字段损坏内部500和业务200，以及下载/初始化/运行时trap等故障。

报告与原始证据位于 `dist/e2e/20261029-rules-final/`。测试命名空间已删除，原有Deployment/EnvoyFilter/ConfigMap配置保持一致。Go全量race检查通过；构建、制品验证单独记录。这些是本地功能结果，不代表生产容量、长期稳定性或生产部署验收。

<details>
<summary>2026-10-28 历史双插件验证记录（旧规则与已废弃统计功能，不作为当前验收）</summary>

## 2026-10-28 修复后验证

在 `kind-kind` 集群中使用 `counter-service:e2e-fixed-v2-20261028-584b7f1` 镜像执行完整测试，34 项全部通过。合法 SSE 的解析错误增量为 0，单个非法帧增量为 1；真实 Redis 停机时，直接 `/acquire` 约 513ms 返回完整 503，网关收到内部 503 后仍约 510ms 返回模拟上游的 200。Redis 恢复后 acquire/release 正常。

同时修复了配置分页对真实 Redis 空表 `{}` 的解码兼容，空页对外返回 `configs: []` 并保留 cursor。测试在 Endpoint 摘除后等待 Redis Pod 完全退出，确保故障注入不受已有连接影响。

三个 Go 模块的全量测试和 race 检查通过：`go test -race -count=1 -p=1 ./shared/... ./rate-limiter/... ./token-stats/...`。本轮报告及原始证据位于 `dist/e2e/20261028-fixed-final/`，测试资源已清理，原有 Deployment、EnvoyFilter 和 ConfigMap 配置与测试前一致。

## 2026-10-28 全链路 fail-open 验证

使用 `counter-service:fail-open-20261028-final` 在同一 `kind-kind` / Istio 1.13.5 集群完成 59 项测试，全部通过。两个网关副本在加载故障切换期间的流量采样没有非 200 或连接异常；请求、响应回调中的真实 Wasm trap 都能继续转发。下载源不可用时网关可以冷启动；Redis 不可用时 Counter Service 可以重启并提供与制品一致的 Wasm。正常并发拒绝与故障恢复也通过验证。

最终报告、流量异常列表和故障配置快照位于 `dist/e2e/20261028-fail-open-verified/`。测试命名空间已删除，原有 Deployment、EnvoyFilter 和 ConfigMap 配置保持一致。全量 Go race 测试通过；镜像内二进制和两个 Wasm、源清单及制品清单中的摘要、归档内容和校验和均已核对。这些是本地功能与制品验证，不代表生产容量、长期稳定性或实际生产部署验收。

</details>
