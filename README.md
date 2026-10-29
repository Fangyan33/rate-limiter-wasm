# WASM 插件部署指南

本仓库包含两个运行在 Istio / Envoy 网关侧的 WASM 插件：

- `rate-limiter`：负责并发限流，支持通过 Counter Service + Redis 实现分布式限流
- `token-stats`：已废弃，停止维护；暂时保留代码、构建和部署清单，后续将完整移除

构建产物由 `bash ./build.sh` 统一生成：

- `dist/rate-limiter.wasm`
- `dist/token-stats.wasm`

以下构建命令从源码仓库根目录执行。接口与匹配规则见[当前限流契约](docs/specs/rate-limiting-contract.md)。历史需求记录已归档到 [docs/specs/PRD.md](docs/specs/PRD.md)，其中未完成或已替代的设计不代表当前能力。

## 统一构建与制品打包

本地需安装 Git、GNU Make、Go、TinyGo，以及构建镜像所需的 Docker。打包还使用 GNU tar、sha256sum、find、sort、xargs 和 sed。Go 版本需满足各模块要求；现有构建使用 TinyGo 0.40.1。

```bash
make version          # 查看版本号
make wasm             # 构建 rate-limiter.wasm 和 token-stats.wasm
make counter-service  # 构建 Linux counter-service 二进制
make build            # 构建二进制和两个 WASM，无需 Docker
make image            # 构建二进制、两个 WASM 和镜像
make artifacts        # 构建全部制品并生成归档，直接 make 也执行此目标

# 指定镜像仓库或目标架构；构建后不会自动推送镜像
make artifacts IMAGE_REPOSITORY=registry.example.com/gateway/counter-service ARCH=arm64
```

默认版本取当前分支 HEAD 可达的最近 Git tag（`git describe --tags --abbrev=0`）。比较 HEAD 与 tag 所指提交的 committer 时间：HEAD 更晚时使用 HEAD 的 12 位短 commit ID，否则使用 tag 原名；没有可达 tag 时使用短 commit ID。轻量 tag 和附注 tag 均支持，附注 tag 的创建时间不参与比较。工作区未提交的修改不会改变版本号。可用 `VERSION=v1.2.3` 手动覆盖；版本号需符合 Docker tag 格式。

默认架构为 `amd64`，所有输出位于 `dist/<版本>/linux-<架构>/`，可用 `DIST_DIR` 修改输出根目录。完整制品包括：

- `counter-service`：Linux 静态二进制。
- `rate-limiter.wasm`、`token-stats.wasm`：与镜像内文件一致的 WASM。
- `counter-service-image.tar`：可用 `docker load` 导入的镜像，镜像名为 `<IMAGE_REPOSITORY>:<版本>`，仓库名默认为 `counter-service`。
- `deploy/istio/`：部署清单副本，其中 Counter Service 镜像版本、两个 EnvoyFilter 的 SHA256 已按本次构建更新。
- `VERSION`、`README.md`、`SHA256SUMS`：版本、使用说明及目录内文件的校验和。
- `docs/`、`scripts/import-service-configs.sh`：随包提供当前契约、说明和配置导入脚本，避免 README 引用缺失。

同时生成 `dist/rate-limiter-wasm-<版本>-linux-<架构>.tar.gz` 及对应的 `.sha256` 文件。解压后可执行：

```bash
sha256sum -c SHA256SUMS
docker load -i counter-service-image.tar
```

部署前仍需审阅清单中的命名空间、网关标签、服务端口及 Redis 连接信息，按下文顺序部署。打包只修改输出目录中的清单副本。原有 `bash ./build.sh` 和直接从源码构建镜像的命令仍可使用。

## 文件说明

### `rate-limiter` 相关

- `deploy/istio/rate-limiter-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 `rate-limiter` WASM 插件
- `deploy/istio/rate-limiter-plugin-config.yaml` - `rate-limiter` 插件配置示例
- `deploy/istio/counter-service-deployment.yaml` - Counter Service 部署配置，用于分布式限流
- `deploy/istio/redis.yaml` - Redis 部署配置，用于存储分布式限流配置和租约状态

### `token-stats` 相关（已废弃）

- `deploy/istio/token-stats-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 `token-stats` WASM 插件
- `deploy/istio/token-stats-plugin-config.yaml` - `token-stats` 插件配置示例
- `deploy/istio/istio-mesh-config-example.yaml` - Istio 1.13 mesh config 完整 ConfigMap 形态示例，用于说明如何在 `istio-system/istio` 中声明 token metrics 所需的原生 labels；应用前必须先与现网 mesh 配置合并

## rate-limiter

`rate-limiter` 按 Kubernetes Service 控制并发，维度为 `命名空间/Service名称 + Service端口 + API Key`。多个入口域名指向同一服务端口时共享额度；不同命名空间、服务或端口的额度隔离。同一服务的不同 subset 共享额度。

Wasm 在请求头阶段读取 Envoy 路由已经选中的 `cluster_name`，发送给 Counter Service。Counter Service 解析服务和端口，再通过 Redis Lua 原子选择规则、清理过期成员和分配租约。请求执行期间由 Wasm 续约，流结束时异步释放。插件不查询 Kubernetes API，不做 DNS 解析，也不承担 API Key 认证职责。

仅保留 Counter Service 分布式模式。Wasm 没有入口域名白名单和本地限流规则；`distributed_limit.enabled: false` 表示直接透传。

### Wasm 配置

实际生效配置内嵌在 `deploy/istio/rate-limiter-envoyfilter.yaml`，独立示例文件不会自动被插件读取：

```yaml
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: counter-service
    acquire_path: /acquire
    release_path: /release
    renew_path: /renew
    timeout_ms: 5000
    lease_ttl_ms: 30000
error_response:
  status_code: 429
  message: Rate limit exceeded
```

`cluster` 指向 Counter Service 的 Envoy cluster，与业务上游的 cluster 不同。`timeout_ms` 控制异步 HTTP 等待，未设置或超过5000ms时使用5000ms；独立看门狗最多每100ms检查一次，恢复仍依赖 Envoy 事件循环正常运行。`lease_ttl_ms` 默认30000ms且必须为正数；续约间隔为TTL的三分之一。TTL是异常回收窗口，不是业务请求的最长时长。

### 服务身份和规则

额度配置只接受 `namespace/name`，例如 `ai/inference`。`port` 必须填写 Service 的 `spec.ports[].port`，不填写 Pod 的 targetPort。例如 `port: 80 → targetPort: 8000`，额度配置应使用80。

VirtualService 的 `destination.host` 可以使用短名或FQDN。位于 `ai` 命名空间的 VirtualService 使用 `host: inference` 时，Istio会解析为 `inference.ai.svc.cluster.local`；Counter Service 将对应 outbound cluster 转为 `ai/inference`。短名称的命名空间由 VirtualService 自身决定。

Counter Service 的 `KUBERNETES_CLUSTER_DOMAIN` 默认 `cluster.local`，可配置其他集群域。仅识别 `outbound|port|subset|service.namespace.svc.<集群域>`；非Kubernetes目标或不能识别的目标正常放行，不访问Redis、不分配租约。

规则顺序为：

1. 服务、端口和具体 API Key。
2. 同一服务端口的空 API Key 默认规则。

不存在或 `enabled=false` 的规则跳过。启用规则一旦命中，超限就拒绝，不再使用默认规则。全部候选无效时放行，不分配租约。API Key 从 `Authorization: Bearer <key>` 提取，缺失或格式错误视为空Key，可使用服务端口默认额度。

具体Key与默认规则使用独立额度池，默认规则供未命中具体Key的请求共享，不是叠加于所有请求的服务总上限。动态更新只影响后续acquire，已有租约继续在原额度池续约和释放。

### 配置管理 API

PUT `/config` 创建或更新一条规则：

```bash
curl --fail-with-body -X PUT http://localhost:8080/config \
  -H 'Content-Type: application/json' \
  -d '{"service":"ai/inference","port":8000,"api_key":"key001","max_concurrent":10,"enabled":true,"description":"推理服务并发额度"}'

# 服务端口默认规则，可省略 api_key
curl --fail-with-body -X PUT http://localhost:8080/config \
  -H 'Content-Type: application/json' \
  -d '{"service":"ai/inference","port":8000,"max_concurrent":20,"enabled":true}'
```

- `service` 必须为合法的小写 `namespace/name`，不接受裸名称、FQDN或通配符。
- `port` 必须为1～65535的整数；`max_concurrent` 必须为正整数，`enabled` 必须显式提供布尔值。
- `api_key` 保持原始字节，不去空格或改大小写。`api_key_hash` 可选，携带时必须严格等于该Key的SHA256小写十六进制摘要。
- `description` 省略时保留原值，非空字符串覆盖，空字符串清空。字段出现时不接受null；非法写入不会修改Redis。
- 接口不包含 `domain` 或 `tier` 字段；未知字段和查询参数返回400。

查询、删除使用同一组身份参数：

```bash
curl --get http://localhost:8080/config \
  --data-urlencode 'service=ai/inference' --data-urlencode 'port=8000' \
  --data-urlencode 'api_key=key001'

curl -X DELETE 'http://localhost:8080/config?service=ai/inference&port=8000&api_key=key001'
curl 'http://localhost:8080/configs?cursor=0&limit=100'
```

GET `/config` 和 `/configs` 的对象包含 `service`、`port`、`api_key_hash`、`max_concurrent`、`enabled`、可选description和更新时间，不返回明文Key。列表只支持cursor/limit；即使某页为空，也要根据返回的cursor继续，直到`"0"`。不存在的单项查询返回404。

编辑具体Key规则时需提供原始Key，不能把查询对象直接PUT回去：查询没有明文Key，返回对象也包含仅供读取的更新时间。批量导入使用JSON数组，每项按PUT契约填写：

```bash
scripts/import-service-configs.sh -f rules.json -u http://localhost:8080
```

脚本依赖jq和curl，逐条导入，失败立即退出；不记录明文API Key。

### 运行时 API 和 Redis

Wasm 调用 POST `/acquire`：

```json
{
  "upstream_cluster": "outbound|8000||inference.ai.svc.cluster.local",
  "api_key": "key001",
  "ttl_ms": 30000
}
```

`upstream_cluster` 必须为1～1024字节，`ttl_ms`必须为正整数。Wasm读取不到目标、目标为空或过长时直接放行。服务转换全部位于Counter Service，不接受请求中的service/port覆盖转换结果。

成功占用额度返回 `allowed=true`、`lease_id`、`max_concurrent` 和 `current_count`；没有匹配规则或目标不能识别时只返回 `{"allowed":true}`。明确超限返回HTTP200、`allowed=false`、`reason=limit_exceeded`。POST `/renew` 接收 `lease_id`、`ttl_ms`，POST `/release` 接收 `lease_id` 和可选 `api_key`；释放与续约均通过租约指针定位额度池。

```text
rl:service:config:<namespace/name>:<port>:<sha256(api_key)>  Hash
rl:service:leases:<namespace/name>:<port>:<sha256(selected_key)>  ZSET
rl:lease:<lease_id>  String，指向额度池，带TTL
```

可通过 `REDIS_KEY_PREFIX` 修改默认 `rl:` 前缀。Redis不保存明文API Key。默认规则使用 `sha256("")`，具体Key规则使用请求Key摘要。配置Hash保存max_concurrent、enabled、description和updated_at。

acquire使用Redis TIME清理过期ZSET成员后判定额度。指针TTL到期不代表ZSET成员自动消失，由下一次acquire清理。release幂等；renew同时延长指针和成员有效期，不复活已经释放或过期的租约。

### 故障放行

业务可用性优先于限流；只允许明确的超限策略拒绝业务。

| 场景 | Counter Service 结果 | 网关行为 |
| --- | --- | --- |
| 启用规则明确超限 | HTTP200，allowed=false，limit_exceeded | 按配置拒绝，默认429 |
| 无启用规则或目标不能识别 | HTTP200，allowed=true，无租约 | 转发，不续约、不释放 |
| Redis配置字段损坏 | HTTP500，invalid_config | 放行，不回退其他规则或本地限流 |
| Redis网络故障或超时 | HTTP503，redis_unavailable | 放行 |
| 调用、解析、续约、释放异常 | 内部错误或无有效结果 | 继续业务请求和响应 |

acquire的Redis连接等待、执行和重试共用500ms预算。Wasm对非200、超时、空体、非法JSON、缺少布尔allowed、未知拒绝原因和超大响应均放行；看门狗恢复暂停请求并忽略迟到拒绝。Counter Service在Redis不可用时仍能启动并提供Wasm下载。

保留ECDS独立加载、Lua透传默认配置、`fail_open: true` 和 `vm_config.nack_on_code_cache_miss: true`。插件业务配置错误记录日志并禁用限流，不阻止监听器就绪。故障放行期间不保证严格并发上限；代理进程或业务上游自身的错误按原有网关行为处理。

### 部署和验证

复用现有Istio网关。仓库网关位于`ai-gateway-ns`，EnvoyFilter位于`istio-system`；确认工作负载标签与selector匹配，并检查Redis地址、认证和集群域配置。

1. 部署Redis及Counter Service，确认`/health`和`/wasm/rate-limiter.wasm`可访问。
2. 通过配置API或导入脚本创建服务端口规则。
3. 发布构建产物中的Wasm及匹配SHA256的EnvoyFilter。
4. 使用真实Service路由保持一个长请求，确认同服务端口的另一个域名请求超限；结束后应恢复。再验证其他端口隔离和依赖故障放行。

```bash
kubectl --context kind-kind apply -f deploy/istio/redis.yaml
kubectl --context kind-kind apply -f deploy/istio/counter-service-deployment.yaml
kubectl --context kind-kind -n default port-forward service/counter-service 8080:8080
# 通过上面的配置API导入规则，再部署已核对下载文件摘要的清单
kubectl --context kind-kind apply -f deploy/istio/rate-limiter-envoyfilter.yaml
```

使用完整制品时，应用解压目录中的渲染清单。`rate-limiter-plugin-config.yaml`是独立示例，修改它不会自动改变EnvoyFilter内嵌配置。

```bash
go test -race -count=1 -p=1 ./shared/... ./rate-limiter/... ./token-stats/...
go test ./rate-limiter/internal/counter-service/models -run '^$' -bench ParseUpstreamCluster -benchmem
```

真实Istio专项步骤见[本地Kubernetes E2E](docs/k8s-e2e.md)。Go测试、制品校验、隔离Istio功能验证和生产容量/稳定性验收分别记录。

排查限流未生效时，先确认插件已加载以及请求路由的实际cluster，再检查Service port与规则是否一致、规则是否启用和Key摘要是否正确。不能用无规则时HTTP200证明插件已激活；应通过实际占满租约后的429验证。

## token-stats（已废弃）

该功能停止维护，后续将完整移除。本次仅保留现有代码、构建和部署清单，不修复 UID 精度、指标基数或解析问题。以下内容为冻结的既有功能说明，不属于当前推荐部署或限流验收要求。

`token-stats` 只负责 usage 统计与 `stream_options.include_usage` 注入，不参与限流、Redis 配置管理或 Counter Service 配置管理。

### 配置示例

```yaml
domains:
  - llm-demo.local
  - vllm-083.default.paic.com.cn

token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
```

### 认证条件

统计和 `stream_options.include_usage` 注入都要求请求命中 `domains`，并携带 `Authorization: Bearer <JWT>`。JWT payload 必须包含有效的 `uid`：支持非空字符串或整数，去掉首尾空格后不超过 64 字节，且不含换行、回车或制表符；Authorization 头不得超过 16 KiB。

JWT 格式错误、缺少 `uid` 或 `uid` 不合法时，该请求仍继续转发，但同时关闭 Token 统计和 usage 注入。普通 `Bearer key_basic_001` 可用于限流，不满足 Token 统计条件；同时启用两个插件且使用具体 key 限流时，应把完整 JWT 作为 API Key 注册到 Counter Service。使用空 key 的域名规则时，无需为限流注册每个 JWT，但 Token 统计仍要求携带有效格式且包含 `uid` 的 JWT。

此处只解析 JWT payload，不校验签名、有效期或颁发方。身份真实性由上游认证链路保障。

### 部署步骤

#### 前置条件

1. Kubernetes 集群已安装 Istio，目标网关的标签与 EnvoyFilter selector 一致。
2. 已构建 WASM 模块：`bash ./build.sh`。
3. 已发布包含最新 `token-stats.wasm` 的 Counter Service 镜像及服务，使网关可下载 `http://counter-service.default.svc.cluster.local:8080/wasm/token-stats.wasm`。下载服务可以在 Redis 不可用时启动；限流依赖 Redis，后端故障时网关放行。
4. 网关中已存在名为 `counter-service` 的 Envoy 集群。当前它由 `rate-limiter-envoyfilter.yaml` 的 CLUSTER patch 创建，Token EnvoyFilter 本身不创建该集群。使用随附清单时需先应用限流 EnvoyFilter；如果只启用 Token 插件，需单独提供等效的下载集群并确认可达，不能只 apply Token EnvoyFilter。
5. 为 Prometheus 指标启用 `domain/uid` labels 时，按后文说明合并 mesh config 的 `extraStatTags`。

可用 `istioctl proxy-config cluster <gateway-pod> -n ai-gateway-ns | grep counter-service` 确认下载集群存在。

#### 步骤 1：更新 WASM 模块 SHA256

计算 WASM 模块的 SHA256：

```bash
sha256sum dist/token-stats.wasm
```

更新 `deploy/istio/token-stats-envoyfilter.yaml` 中的 `sha256` 字段。

#### 步骤 2：部署 EnvoyFilter

```bash
kubectl apply -f deploy/istio/token-stats-envoyfilter.yaml
```

#### 步骤 3：验证部署

检查 Envoy 配置是否生效：

```bash
istioctl proxy-config listener <gateway-pod> -n ai-gateway-ns
```

检查 WASM 模块是否加载：

```bash
kubectl logs <gateway-pod> -n ai-gateway-ns | grep -i wasm
```

### 监控指标

#### Istio 1.13 token metrics

当前，`token-stats` 在 `istio-ingressgateway` 中会暴露如下指标：

```text
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```

#### Istio 1.13 token metrics 标签说明

对于 Istio 1.13.5，只有部署 `deploy/istio/token-stats-envoyfilter.yaml` 还不足以让 Prometheus 中的 token metrics 自动带上 `domain` 和 `uid` labels。这个版本需要在 mesh config 中通过 `defaultConfig.extraStatTags` 显式声明这两个 tag，Envoy 才会把插件输出的统计维度作为原生 label 暴露出来。

已废弃功能的既有配置方式如下：

1. 部署 `token-stats` 的 EnvoyFilter，使插件产生对应统计维度。
2. 同时在 Istio mesh config 中声明 `defaultConfig.extraStatTags: [domain, uid]`。
3. 参考 `deploy/istio/istio-mesh-config-example.yaml`，把 `defaultConfig.extraStatTags` 合并进现有 mesh 配置。

需要特别说明：

- 自定义 EnvoyFilter BOOTSTRAP regex 不是 Istio 1.13.5 获取这些 labels 的支持路径。
- Prometheus 中的 metric name 不需要改名；完成 mesh config 声明后，原有 metric 会直接带出 `domain` 和 `uid` labels。
- 旧的 `stats_config`、`stats_tags` 或基于 `__host0domain__`、`__user0id__` 的方案仅可作为历史背景说明，不应视为当前部署指令。

#### Istio 1.13 mesh config 示例

应更新现网 `istio-system/istio` 的 `data.mesh`：将 `defaultConfig.extraStatTags` 中的 `domain`、`uid` 合并进当前 mesh 配置，而不是把示例文件当作现成部署清单直接 apply。

推荐流程：

1. 先导出并审阅当前集群中的 `istio-system/istio` ConfigMap。
2. 在现有 `data.mesh` 中补充：

   ```yaml
   defaultConfig:
     extraStatTags:
       - domain
       - uid
   ```

3. 确认没有覆盖掉现网已有的 mesh 配置项后，再把合并后的结果回写到集群。
4. `deploy/istio/istio-mesh-config-example.yaml` 仅作为完整 `ConfigMap` 形态参考，用来帮助定位 `data.mesh` 的写法，不是当前推荐的直接 apply 主路径。

需要特别说明：

- `istio-mesh-config-example.yaml` 中的 `accessLogFile`、`enablePrometheusMerge`、`trustDomain` 等字段只是为了展示一个更完整的 `ConfigMap` 形态；它们不是 token metrics labels 的最小必需项。
- 对当前需求，关键依赖仍然只有把 `defaultConfig.extraStatTags` 中的 `domain`、`uid` 合并进现网 mesh 配置。
- 自定义 EnvoyFilter BOOTSTRAP regex 仅作为历史说明，不是当前部署指令。

也就是说：EnvoyFilter-only 仍然不够，`extraStatTags` 仍然是当前方案的显式依赖；而完整示例文件的作用是“参考合并形态”，不是“直接覆盖 apply”。
