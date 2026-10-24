# WASM 插件部署指南

本仓库包含两个运行在 Istio / Envoy 网关侧的 WASM 插件：

- `rate-limiter`：负责并发限流，支持通过 Counter Service + Redis 实现分布式限流
- `token-stats`：负责 LLM token usage 统计与 stream usage 注入，不参与限流

构建产物由 `bash ./build.sh` 统一生成：

- `dist/rate-limiter.wasm`
- `dist/token-stats.wasm`

以下构建命令从仓库根目录执行。历史需求记录已归档到 [docs/specs/PRD.md](docs/specs/PRD.md)，其中未完成或已替代的设计不代表当前能力。

## 文件说明

### `rate-limiter` 相关

- `deploy/istio/rate-limiter-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 `rate-limiter` WASM 插件
- `deploy/istio/rate-limiter-plugin-config.yaml` - `rate-limiter` 插件配置示例
- `deploy/istio/counter-service-deployment.yaml` - Counter Service 部署配置，用于分布式限流
- `deploy/istio/redis.yaml` - Redis 部署配置，用于存储分布式限流配置和租约状态

### `token-stats` 相关

- `deploy/istio/token-stats-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 `token-stats` WASM 插件
- `deploy/istio/token-stats-plugin-config.yaml` - `token-stats` 插件配置示例
- `deploy/istio/istio-mesh-config-example.yaml` - Istio 1.13 mesh config 完整 ConfigMap 形态示例，用于说明如何在 `istio-system/istio` 中声明 token metrics 所需的原生 labels；应用前必须先与现网 mesh 配置合并

## rate-limiter

`rate-limiter` 负责请求并发控制。当前推荐使用分布式限流模式：插件通过异步 HTTP 调用访问 Counter Service，限流配置由 Redis 动态存储和管理。

### 部署模式

#### 分布式限流模式（Counter Service + Redis）

使用异步 HTTP 调用与 Counter Service 通信，实现跨多个 Envoy 实例的分布式限流。限流配置存储在 Redis 中，支持动态更新，无需重启插件。

配置示例：

```yaml
domains:
  - llm-demo.local
  - vllm-083.default.paic.com.cn
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

**注意：** 分布式模式下，`rate_limits` 配置项已废弃。所有限流配置通过 Counter Service 的配置管理 API 动态管理，存储在 Redis 中。

#### Counter Service 配置说明

- `cluster`: Envoy 集群名称，指向 Counter Service
- `acquire_path`: 获取限流槽位的 API 路径
- `release_path`: 释放限流槽位的 API 路径
- `renew_path`: 续约 API 路径，默认 `/renew`
- `timeout_ms`: HTTP 调用超时时间（毫秒）
- `lease_ttl_ms`: 租约 TTL（毫秒），防止槽位泄漏

#### Redis 配置存储架构

限流配置在 Redis 中按 `domain + sha256(api_key)` 维度存储为 Hash。Counter Service 对外接口仍然接收明文 `api_key`，但 Redis 内部不会保存明文 key。

**配置键格式：** `rl:config:{domain}:{api_key_sha256}`

**配置字段：**

- `max_concurrent`: 最大并发数
- `enabled`: 是否启用（true/false）
- `tier`: 可选，用户层级标识（如 basic, premium, enterprise）

`api_key` 为空字符串或省略时表示域名规则，使用 `sha256("")` 存储。域名规则下，同一实际域名的所有请求共享并发额度，包括无 Authorization、格式错误的 Authorization 和携带任意 API Key 的请求。

**规则匹配优先级：**

先查空 key 的域名规则，顺序为精确域名 → 父域名通配 → 全局通配；只有这三类规则全部不存在时，才按以下顺序查找请求中的具体 API Key：

1. 精确匹配：`rl:config:api.example.com:<sha256(key001)>`
2. 父域名通配：`rl:config:*.example.com:<sha256(key001)>`
3. 全局通配：`rl:config:*:<sha256(key001)>`

域名通配规则也优先于精确域名的具体 key 规则。命中规则若被禁用、阈值无效或并发已满，直接拒绝，不再回退；禁用规则仍沿用 `api_key_disabled` 拒绝原因。通配符只查当前父域（例如 `a.b.example.com` 查 `*.b.example.com`）和全局 `*`，计数按实际请求域名隔离，不同子域名不会共享额度。插件的 `domains` 仍决定哪些请求进入限流。

#### 仅按域名限流

例如，将 `api.example.com` 的总并发数限制为 10：

```bash
curl -X PUT http://counter-service:8080/config \
  -H "Content-Type: application/json" \
  -d '{"domain":"api.example.com","api_key":"","max_concurrent":10,"enabled":true}'

# 空 api_key 也可以省略；查询和删除时同样适用
curl "http://counter-service:8080/config?domain=api.example.com"
curl -X DELETE "http://counter-service:8080/config?domain=api.example.com&api_key="
```

配置生效后，带不同 key 的请求和不带 key 的请求合计最多同时运行 10 个；流式请求在结束前持续占用额度。没有匹配的域名规则时，仍须匹配有效 Bearer 中的具体 key 配置，否则拒绝。

本地静态模式使用如下配置：

```yaml
domains:
  - api.example.com
  - other.example.com
rate_limits:
  - api_key: ""
    max_concurrent: 10
distributed_limit:
  enabled: false
```

两个域名各自拥有 10 个并发槽位。每份本地配置只能包含一条空 key 规则，该规则优先于同一配置中的具体 key 规则。本地额度仅在当前插件实例内生效；未配置空 key 时，原有按 key 的计数行为不变。不将非空 key 自动去空格后当作域名规则。

升级时先发布 Counter Service，再发布 WASM 插件，最后创建空 key 规则。无需迁移已有 Redis 数据；规则切换仅影响后续 acquire，已有租约仍在原集合中续约和释放，不追溯合并。因此切换期间已有请求不会自动计入新的域名额度。删除域名规则后，后续请求恢复使用具体 key 规则。

#### 配置管理 API

Counter Service 提供 RESTful API 用于管理限流配置：

**创建/更新配置：**

```bash
curl -X PUT http://counter-service:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "llm-demo.local",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": true,
    "tier": "basic"
  }'
```

**查询单个配置：**

```bash
curl "http://counter-service:8080/config?domain=llm-demo.local&api_key=key_basic_001"
```

返回结果会包含请求中的明文 `api_key`，以及对应的 `api_key_hash`。

**分页查询配置：**

```bash
curl "http://counter-service:8080/configs?cursor=0&limit=100"
```

`/configs` 返回 `cursor` 和当前页的 `configs`。将返回的 `cursor` 作为下一次请求的参数，直到返回 `"0"` 才表示本轮遍历结束；一页不足以代表全部配置。列表不会返回明文 `api_key`，只返回 `api_key_hash`。

**删除配置：**

```bash
curl -X DELETE "http://counter-service:8080/config?domain=llm-demo.local&api_key=key_basic_001"
```

#### Counter Service API 契约

**Acquire 请求：**

```http
POST /acquire
Content-Type: application/json

{
  "domain": "llm-demo.local",
  "api_key": "key_basic_001",
  "ttl_ms": 30000
}
```

**Acquire 响应（成功）：**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "allowed": true,
  "lease_id": "lease-uuid-123",
  "max_concurrent": 2,
  "current_count": 1,
  "tier": "basic"
}
```

**Acquire 响应（拒绝 - 超限）：**

这里描述的是插件调用的 Counter Service `/acquire` 接口。网关对业务请求的拒绝仍使用 `error_response`，默认是 HTTP 429 和纯文本消息。当前 `/acquire` 拒绝响应不包含 `max_concurrent`、`current_count`。

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "allowed": false,
  "reason": "limit_exceeded",
  "message": "limit exceeded"
}
```

**Acquire 响应（拒绝 - 配置未找到）：**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "allowed": false,
  "reason": "config_not_found",
  "message": "config not found"
}
```

**Acquire 响应（拒绝 - Redis 不可用）：**

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{
  "allowed": false,
  "reason": "redis_unavailable",
  "message": "redis unavailable"
}
```

**Release 请求：**

```http
POST /release
Content-Type: application/json

{
  "lease_id": "lease-uuid-123"
}
```

**Release 响应：**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "released": true
}
```

**Release 响应（租约不存在）：**

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "released": false,
  "reason": "lease_not_found",
  "message": "lease not found"
}
```

**Release 响应（Redis 不可用）：**

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{
  "released": false,
  "reason": "redis unavailable",
  "message": "redis unavailable"
}
```

Release 响应会被插件忽略（best-effort 释放）；插件只尝试发送 release，不依赖响应体继续处理请求。

**Renew 请求：**

```http
POST /renew
Content-Type: application/json

{
  "lease_id": "lease-uuid-123",
  "ttl_ms": 30000
}
```

有效租约续约成功返回 `HTTP 200 + {"renewed":true}`。不存在、已过期或已释放的租约返回 `HTTP 200 + {"renewed":false,"reason":"lease_not_found"}`，不会重新占用槽位。参数非法返回 400，Redis 不可用返回 `503 + renewed=false + reason=redis_unavailable`，其他服务端错误返回 `500 + renewed=false + reason=internal_error`。

续约由插件的根上下文定时触发，默认每 `lease_ttl_ms / 3` 续约一次，按毫秒取整且至少为 1ms。默认 TTL 为 30 秒，因此正常每 10 秒续约一次；请求结束后停止续约并尝试释放。续约调用超时不超过配置的 `timeout_ms`（未设置时为 5000ms）、续约间隔和租约剩余有效时间中的最小值。

### 部署步骤

#### 前置条件

1. Kubernetes 集群已安装 Istio
2. 已构建 WASM 模块：`bash ./build.sh`
3. 已构建 counter-service 镜像

```bash
docker build -t counter-service:latest -f rate-limiter/Dockerfile.counter-service .
```

升级已有部署时，先发布支持 `/renew` 的 Counter Service，再发布新的 `rate-limiter.wasm` 并更新 EnvoyFilter SHA256。Redis 键结构无需迁移。镜像内置两个 WASM 文件，修改插件后需重新构建并发布镜像，确保下载服务中的文件与 EnvoyFilter SHA256 对应。

仓库的 `ai-gateway` 位于 `ai-gateway-ns`；EnvoyFilter 清单位于 `istio-system`，mesh ConfigMap 为 `istio-system/istio`。如果实际部署位置不同，下面的排查命令应使用对应资源所在的命名空间。

#### 步骤 1：部署 Redis（分布式模式必需）

如果使用分布式限流模式，需要先部署 Redis：

```bash
kubectl apply -f deploy/istio/redis.yaml
```

确保 Redis 正常运行：

```bash
kubectl get pods -l app=redis
kubectl logs -l app=redis
```

#### 步骤 2：部署 Counter Service（分布式模式必需）

部署 Counter Service：

```bash
kubectl apply -f deploy/istio/counter-service-deployment.yaml
```

确保 Counter Service 正常运行：

```bash
kubectl get pods -l app=counter-service
kubectl logs -l app=counter-service

# 检查健康状态
kubectl exec <counter-service-pod> -n default -- wget -qO- http://localhost:8080/health
```

#### 步骤 3：导入限流配置到 Redis

##### 方法 1：使用迁移脚本（推荐）

迁移脚本需要 `yq`（mikefarah 版本）、`jq` 和 `curl`。默认的 `deploy/istio/rate-limiter-plugin-config.yaml` 中 `rate_limits` 全部被注释，直接使用默认文件会提示没有配置并退出，不会导入数据。

可先准备独立的迁移输入 `rate-limits-import.yaml`：

```yaml
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 2
    tier: basic
  - api_key: key_premium_001
    max_concurrent: 5
    tier: premium
```

也可以取消默认文件中的 `rate_limits` 注释并填写实际配置；分布式模式仍通过 Counter Service 读取 Redis 中的阈值。下面用独立文件导入与随附 EnvoyFilter 一致的域名：

```bash
# 安装 yq（如果未安装）
# macOS: brew install yq
# Linux: wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq && chmod +x /usr/local/bin/yq

# 指定迁移输入和可访问的 Counter Service URL
bash scripts/migrate-config-to-redis.sh \
  -f rate-limits-import.yaml \
  -u http://localhost:8080 \
  -d "llm-demo.local"
```

在集群外操作时，可先运行 `kubectl port-forward -n default svc/counter-service 8080:8080`，保持该进程运行，再使用上面的 `localhost:8080`。脚本的最终汇总计数存在管道子 shell 导致始终为 0 的已知问题，应以每条导入结果及后续 `/config` 查询为准。

##### 方法 2：手动导入配置

使用 Counter Service 配置管理 API 手动导入：

```bash
# 基础用户配置
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "llm-demo.local",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": true,
    "tier": "basic"
  }'

# 高级用户配置
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "llm-demo.local",
    "api_key": "key_premium_001",
    "max_concurrent": 5,
    "enabled": true,
    "tier": "premium"
  }'

# 通配符域名配置（作为 fallback）
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "*.example.com",
    "api_key": "key_basic_001",
    "max_concurrent": 3,
    "enabled": true,
    "tier": "basic"
  }'
```

通配符示例只展示 Redis 配置回退规则。若要通过网关验证 `*.example.com`，需先把对应域名模式加入 EnvoyFilter 内嵌的 `domains`；当前随附清单只拦截 `llm-demo.local` 和 `vllm-083.default.paic.com.cn`。

##### 验证配置

```bash
# 查询第一页；按响应 cursor 继续翻页，直到 cursor="0"
curl "http://<counter-service-ip>:8080/configs?cursor=0&limit=100"

# 查询特定配置
curl "http://<counter-service-ip>:8080/config?domain=llm-demo.local&api_key=key_basic_001"
```

注意：

- `/config` 单项查询返回 `api_key` 和 `api_key_hash`
- `/configs` 列表只返回 `api_key_hash`，不会回显明文 `api_key`

#### 步骤 4：更新 WASM 模块 SHA256

计算 WASM 模块的 SHA256：

```bash
sha256sum dist/rate-limiter.wasm
```

更新 `deploy/istio/rate-limiter-envoyfilter.yaml` 中的 `sha256` 字段。

#### 步骤 5：部署 EnvoyFilter

```bash
kubectl apply -f deploy/istio/rate-limiter-envoyfilter.yaml
```

#### 步骤 6：验证部署

检查 Envoy 配置是否生效：

```bash
istioctl proxy-config listener <gateway-pod> -n ai-gateway-ns
```

发送测试请求：

下面的域名与随附 EnvoyFilter 一致，需先为该域名和 API Key 导入配置，并准备可用的网关路由与上游。

```bash
curl -H "Host: llm-demo.local" \
     -H "Authorization: Bearer key_basic_001" \
     http://<gateway-ip>/test
```

### 动态配置管理

分布式模式下，限流配置存储在 Redis 中，可通过 Counter Service API 实时修改，无需重启插件。

#### 修改配置

```bash
# 调整并发限制
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "llm-demo.local",
    "api_key": "key_basic_001",
    "max_concurrent": 10,
    "enabled": true,
    "tier": "basic"
  }'
```

配置变更立即生效，下一个请求将使用新的限制值。

#### 禁用/启用 API Key

```bash
# 禁用某个 API Key
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "llm-demo.local",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": false
  }'
```

禁用后，`POST /acquire` 会返回 `HTTP 200 + allowed=false + reason=api_key_disabled`；网关侧请求会按插件 `error_response` 被拒绝（默认 429）。

#### 删除配置

```bash
curl -X DELETE "http://<counter-service-ip>:8080/config?domain=llm-demo.local&api_key=key_basic_001"
```

#### 批量导入配置

可以编写脚本批量导入配置：

```bash
#!/bin/bash
# import-configs.sh

COUNTER_SERVICE="http://counter-service:8080"

# 从 JSON 文件读取配置
cat configs.json | jq -c '.[]' | while read config; do
  curl -X PUT "$COUNTER_SERVICE/config" \
    -H "Content-Type: application/json" \
    -d "$config"
  echo "Imported: $config"
done
```

`configs.json` 示例：

```json
[
  {
    "domain": "llm-demo.local",
    "api_key": "key001",
    "max_concurrent": 5,
    "enabled": true,
    "tier": "premium"
  },
  {
    "domain": "*.example.com",
    "api_key": "key002",
    "max_concurrent": 2,
    "enabled": true,
    "tier": "basic"
  }
]
```

### 降级行为

当 Counter Service 调用失败、超时、返回非 `200`，或返回体无法解析时，插件当前行为是 **fail-open 放行当前请求**，不会切回本地限流。缺少或格式错误的 Bearer 凭证也采用相同策略；服务正常时仍必须匹配限流规则才能通过。

当 Counter Service 返回 `HTTP 200` 且 `allowed=false` 时，插件会按 `error_response` 配置直接拒绝请求。

续约发起失败、超时、非 200 或响应不可解析时，插件保持业务请求继续，在租约有效期内按后续周期重试；同一租约最多有一个未完成的续约调用。确认租约不存在或超过保守有效期后停止续约并记录 `lease renewal stopped` 告警，不重新 acquire，也不中断业务请求。因此，持续后端故障或调度延迟超过 TTL 时，允许暂时突破并发上限。插件定时器初始化失败时记录错误并对分布式请求 fail-open，避免获取无法维护的租约。

### 配置调优

#### Timeout 设置

- `timeout_ms`: 建议设置为 1000-5000ms
  - 太短：容易触发降级
  - 太长：影响请求延迟

#### Lease TTL 设置

- `lease_ttl_ms`: 建议设置为 30000-60000ms（30-60秒）
  - 太短：频繁续约，增加 Counter Service 负载
  - 太长：异常情况下槽位泄漏时间长
  - TTL 应覆盖续约调用、网络抖动和 Envoy 调度延迟；它是故障回收窗口，不是业务请求的最长时长。正常续约时，长请求可以跨多个 TTL 保持占用。

发布前应在真实 Istio 网关上验证：将并发上限设为 1，保持一个请求运行超过三个 TTL，确认第二个请求持续被拒绝；第一个请求结束后确认新请求立即可用；再验证 Redis 故障期间业务请求继续、出现续约失败日志，并在故障恢复后恢复新请求的分布式判定。本地测试不替代这些部署验收。

#### Counter Service 副本数

根据流量规模调整 Counter Service 副本数：

- 低流量（< 1000 RPS）：2 副本
- 中流量（1000-10000 RPS）：3-5 副本
- 高流量（> 10000 RPS）：5+ 副本 + 水平扩展

### 故障排查

#### 插件未生效

1. 检查 EnvoyFilter 是否应用成功：

   ```bash
   kubectl get envoyfilter -n istio-system
   ```

2. 检查 WASM 模块是否加载：

   ```bash
   kubectl logs <gateway-pod> -n ai-gateway-ns | grep -i wasm
   ```

#### 限流不工作

1. 检查域名匹配：

   ```bash
   # 确保请求的 Host 头匹配配置中的 domains
   curl -v -H "Host: llm-demo.local" ...
   ```

2. 检查 API Key：

   ```bash
   # 使用具体 key 规则时，确保 Authorization 头格式正确；域名规则不要求该头
   curl -v -H "Authorization: Bearer key_basic_001" ...
   ```

3. 查看插件日志：

   ```bash
   kubectl logs <gateway-pod> -n ai-gateway-ns | grep -i "rate"
   ```

#### Counter Service 连接失败

1. 检查 Counter Service 是否运行：

   ```bash
   kubectl get pods -l app=counter-service
   ```

2. 检查网络连通性：

   ```bash
   kubectl exec <gateway-pod> -n ai-gateway-ns -- \
     curl -v http://counter-service.default.svc.cluster.local:8080/health
   ```

3. 检查 Envoy 集群配置：

   ```bash
   istioctl proxy-config cluster <gateway-pod> -n ai-gateway-ns | grep counter-service
   ```

## token-stats

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
3. 已发布包含最新 `token-stats.wasm` 的 Counter Service 镜像及服务，使网关可下载 `http://counter-service.default.svc.cluster.local:8080/wasm/token-stats.wasm`。当前下载服务启动依赖 Redis，因此也需完成 Redis 和 Counter Service 部署。
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

当前推荐做法如下：

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
