# Istio 部署指南

本目录包含在 Istio 环境中部署 rate-limiter WASM 插件的配置示例。

## 文件说明

- `rate-limiter-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 WASM 插件
- `rate-limiter-plugin-config.yaml` - 插件配置示例（独立配置文件）
- `counter-service-deployment.yaml` - Counter Service 部署配置（用于分布式限流）
- `istio-mesh-config-example.yaml` - Istio 1.13 mesh config 完整 ConfigMap 形态示例，用于说明如何在 `istio-system/istio` 中声明 token metrics 所需的原生 labels；应用前必须先与现网 mesh 配置合并

## Istio 1.13 token metrics 标签说明

对于 Istio 1.13.5，只有部署 `rate-limiter-envoyfilter.yaml` 还不足以让 Prometheus 中的 token metrics 自动带上 `domain` 和 `uid` labels。这个版本需要在 mesh config 中通过 `defaultConfig.extraStatTags` 显式声明这两个 tag，Envoy 才会把插件输出的统计维度作为原生 label 暴露出来。

当前推荐做法如下：

1. 部署本目录的 EnvoyFilter，使 WASM 插件产生对应统计维度。
2. 同时在 Istio mesh config 中声明 `defaultConfig.extraStatTags: [domain, uid]`。
3. 可参考新增的 `istio-mesh-config-example.yaml`，它提供了适用于 Istio 1.13 的完整 `ConfigMap` 形态示例，便于把 `defaultConfig.extraStatTags` 合并进现有 mesh 配置。

需要特别说明：

- 自定义 EnvoyFilter BOOTSTRAP regex 不是 Istio 1.13.5 获取这些 labels 的支持路径。
- Prometheus 中的 metric name 不需要改名；完成 mesh config 声明后，原有 metric 会直接带出 `domain` 和 `uid` labels。
- 旧的 `stats_config`、`stats_tags` 或基于 `__host0domain__`、`__user0id__` 的方案仅可作为历史背景说明，不应视为当前部署指令。

### Istio 1.13 mesh config 示例

active guidance 应聚焦在更新现网 `istio-system/istio` 的 `data.mesh`：将 `defaultConfig.extraStatTags` 中的 `domain`、`uid` 合并进当前 mesh 配置，而不是把示例文件当作现成部署清单直接 apply。

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

### 历史方案说明（非当前方案）

历史排查中可能会见到通过 `stats_config`、`stats_tags` 或自定义 BOOTSTRAP regex 注入标签的做法。这些路径在当前目标版本中不作为正式支持方案，本目录新增的 mesh config example 才是当前依赖项示例。

## 部署模式

### 1. 本地限流模式

仅在单个 Envoy 实例内进行限流，不需要外部服务。

配置示例：
```yaml
domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 2
  - api_key: key_premium_001
    max_concurrent: 5
distributed_limit:
  enabled: false
error_response:
  status_code: 429
  message: Rate limit exceeded
```

### 2. 分布式限流模式（Counter Service + Redis）

使用异步 HTTP 调用与 Counter Service 通信，实现跨多个 Envoy 实例的分布式限流。限流配置存储在 Redis 中，支持动态更新，无需重启插件。

配置示例：
```yaml
domains:
  - api.example.com
  - "*.example.com"  # 支持通配符域名
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ratelimit-service
    acquire_path: /acquire
    release_path: /release
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
- `timeout_ms`: HTTP 调用超时时间（毫秒）
- `lease_ttl_ms`: 租约 TTL（毫秒），防止槽位泄漏

#### Redis 配置存储架构

限流配置按 `domain + api_key` 维度存储在 Redis Hash 中：

**配置键格式：** `rl:config:{domain}:{api_key}`

**配置字段：**
- `max_concurrent`: 最大并发数
- `enabled`: 是否启用（true/false）
- `tier`: 可选，用户层级标识（如 basic, premium, enterprise）

**通配符匹配优先级：**
1. 精确匹配：`rl:config:api.example.com:key001`
2. 父域名通配：`rl:config:*.example.com:key001`
3. 全局通配：`rl:config:*:key001`

#### 配置管理 API

Counter Service 提供 RESTful API 用于管理限流配置：

**创建/更新配置：**
```bash
curl -X PUT http://counter-service:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "api.example.com",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": true,
    "tier": "basic"
  }'
```

**查询单个配置：**
```bash
curl "http://counter-service:8080/config?domain=api.example.com&api_key=key_basic_001"
```

**列出所有配置：**
```bash
curl "http://counter-service:8080/configs"
```

**删除配置：**
```bash
curl -X DELETE "http://counter-service:8080/config?domain=api.example.com&api_key=key_basic_001"
```

#### Counter Service API 契约

**Acquire 请求：**
```json
POST /acquire
Content-Type: application/json

{
  "domain": "api.example.com",
  "api_key": "key_basic_001",
  "ttl_ms": 30000
}
```

**Acquire 响应（成功）：**
```json
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
```json
HTTP/1.1 429 Too Many Requests
Content-Type: application/json

{
  "allowed": false,
  "reason": "limit_exceeded",
  "max_concurrent": 2,
  "current_count": 2
}
```

**Acquire 响应（拒绝 - 配置未找到）：**
```json
HTTP/1.1 404 Not Found
Content-Type: application/json

{
  "allowed": false,
  "reason": "config_not_found"
}
```

**Release 请求：**
```json
POST /release
Content-Type: application/json

{
  "lease_id": "lease-uuid-123"
}
```

**Release 响应：**
```json
HTTP/1.1 200 OK
Content-Type: application/json

{
  "released": true
}
```

Release 响应会被插件忽略（best-effort 释放）。

## 部署步骤

### 前置条件

1. Kubernetes 集群已安装 Istio
2. 已构建 WASM 模块：`bash ./build.sh`
3. WASM 模块已上传到可访问的 HTTP 服务器

### 步骤 1：部署 Redis（分布式模式必需）

如果使用分布式限流模式，需要先部署 Redis：

```bash
kubectl apply -f redis.yaml
```

确保 Redis 正常运行：
```bash
kubectl get pods -l app=redis
kubectl logs -l app=redis
```

### 步骤 2：部署 Counter Service（分布式模式必需）

部署 Counter Service：

```bash
kubectl apply -f counter-service-deployment.yaml
```

确保 Counter Service 正常运行：
```bash
kubectl get pods -l app=ratelimit-service
kubectl logs -l app=ratelimit-service

# 检查健康状态
kubectl exec -it <counter-service-pod> -- curl http://localhost:8080/health
```

### 步骤 3：导入限流配置到 Redis

#### 方法 1：使用迁移脚本（推荐）

如果已有 YAML 格式的 `rate_limits` 配置，可使用迁移脚本自动导入：

```bash
# 安装 yq（如果未安装）
# macOS: brew install yq
# Linux: wget https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -O /usr/local/bin/yq && chmod +x /usr/local/bin/yq

# 执行迁移（使用默认配置文件）
bash scripts/migrate-config-to-redis.sh

# 或指定配置文件和 Counter Service URL
bash scripts/migrate-config-to-redis.sh \
  -f deploy/istio/rate-limiter-plugin-config.yaml \
  -u http://counter-service:8080 \
  -d "api.example.com"
```

#### 方法 2：手动导入配置

使用 Counter Service 配置管理 API 手动导入：

```bash
# 基础用户配置
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "api.example.com",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": true,
    "tier": "basic"
  }'

# 高级用户配置
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "api.example.com",
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

#### 验证配置

```bash
# 列出所有配置
curl http://<counter-service-ip>:8080/configs

# 查询特定配置
curl "http://<counter-service-ip>:8080/config?domain=api.example.com&api_key=key_basic_001"
```

### 步骤 4：更新 WASM 模块 SHA256

计算 WASM 模块的 SHA256：
```bash
sha256sum dist/rate-limiter.wasm
```

更新 `rate-limiter-envoyfilter.yaml` 中的 `sha256` 字段。

### 步骤 5：部署 EnvoyFilter

```bash
kubectl apply -f rate-limiter-envoyfilter.yaml
```

### 步骤 6：验证部署

检查 Envoy 配置是否生效：
```bash
istioctl proxy-config listener <gateway-pod> -n istio-system
```

发送测试请求：
```bash
curl -H "Host: api.example.com" \
     -H "Authorization: Bearer key_basic_001" \
     http://<gateway-ip>/test
```

## 动态配置管理

分布式模式下，限流配置存储在 Redis 中，可通过 Counter Service API 实时修改，无需重启插件。

### 修改配置

```bash
# 调整并发限制
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "api.example.com",
    "api_key": "key_basic_001",
    "max_concurrent": 10,
    "enabled": true,
    "tier": "basic"
  }'
```

配置变更立即生效，下一个请求将使用新的限制值。

### 禁用/启用 API Key

```bash
# 禁用某个 API Key
curl -X PUT http://<counter-service-ip>:8080/config \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "api.example.com",
    "api_key": "key_basic_001",
    "max_concurrent": 2,
    "enabled": false
  }'
```

禁用后，该 API Key 的所有请求将被拒绝（404 config_not_found）。

### 删除配置

```bash
curl -X DELETE "http://<counter-service-ip>:8080/config?domain=api.example.com&api_key=key_basic_001"
```

### 批量导入配置

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

configs.json 示例：
```json
[
  {
    "domain": "api.example.com",
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

## 降级行为

当 Counter Service 或 Redis 不可用时，插件会自动降级到本地限流模式：

1. HTTP 调用失败或超时 → 使用本地 limiter
2. Counter Service 返回 503（Redis 不可用）→ 使用本地 limiter
3. 响应解析失败 → 使用本地 limiter

降级期间会记录警告日志，可通过 Envoy 日志查看：
```bash
kubectl logs <gateway-pod> -n istio-system | grep -i "falling back to local limiter"
```

**注意：** 降级到本地模式时，如果插件配置中没有 `rate_limits` 静态配置，将拒绝所有请求。建议保留少量静态配置作为降级 fallback。

## 配置调优

### Timeout 设置

- `timeout_ms`: 建议设置为 1000-5000ms
  - 太短：容易触发降级
  - 太长：影响请求延迟

### Lease TTL 设置

- `lease_ttl_ms`: 建议设置为 30000-60000ms（30-60秒）
  - 太短：频繁续约，增加 Counter Service 负载
  - 太长：异常情况下槽位泄漏时间长

### Counter Service 副本数

根据流量规模调整 Counter Service 副本数：
- 低流量（< 1000 RPS）：2 副本
- 中流量（1000-10000 RPS）：3-5 副本
- 高流量（> 10000 RPS）：5+ 副本 + 水平扩展

## 故障排查

### 插件未生效

1. 检查 EnvoyFilter 是否应用成功：
   ```bash
   kubectl get envoyfilter -n istio-system
   ```

2. 检查 WASM 模块是否加载：
   ```bash
   kubectl logs <gateway-pod> -n istio-system | grep -i wasm
   ```

### 限流不工作

1. 检查域名匹配：
   ```bash
   # 确保请求的 Host 头匹配配置中的 domains
   curl -v -H "Host: api.example.com" ...
   ```

2. 检查 API Key：
   ```bash
   # 确保 Authorization 头格式正确
   curl -v -H "Authorization: Bearer key_basic_001" ...
   ```

3. 查看插件日志：
   ```bash
   kubectl logs <gateway-pod> -n istio-system | grep -i "rate"
   ```

### Counter Service 连接失败

1. 检查 Counter Service 是否运行：
   ```bash
   kubectl get pods -l app=ratelimit-service
   ```

2. 检查网络连通性：
   ```bash
   kubectl exec <gateway-pod> -n istio-system -- \
     curl -v http://ratelimit-service.default.svc.cluster.local:8080/health
   ```

3. 检查 Envoy 集群配置：
   ```bash
   istioctl proxy-config cluster <gateway-pod> -n istio-system | grep ratelimit
   ```

## 监控指标

建议监控以下指标：

### Counter Service 指标

如果部署了 Prometheus，可以通过 ServiceMonitor 采集 Counter Service 指标：

```bash
kubectl apply -f prometheus-servicemonitor.yaml
```

关键指标：
- `rate_limit_acquire_total{result="allowed|limit_exceeded|config_not_found|disabled"}` - Acquire 请求结果统计
- `rate_limit_config_lookups_total{result="hit|wildcard|miss"}` - 配置查找结果统计
- `redis_command_duration_seconds` - Redis 命令执行延迟
- `http_request_duration_seconds{path="/acquire"}` - Acquire 请求延迟
- `http_request_duration_seconds{path="/release"}` - Release 请求延迟

### Envoy 指标

通过 Envoy 的统计信息获取插件相关指标：
```bash
kubectl exec <gateway-pod> -n istio-system -- \
  curl -s http://localhost:15000/stats | grep rate_limiter
```

关键指标：
- Counter Service 响应时间
- Counter Service 错误率
- 降级到本地限流的频率
- 429 响应数量
- 每个 API Key 的并发请求数

### 告警建议

建议配置以下告警规则：

1. **Counter Service 不可用**
   ```yaml
   - alert: CounterServiceDown
     expr: up{job="counter-service"} == 0
     for: 1m
     annotations:
       summary: "Counter Service is down"
   ```

2. **Redis 连接失败率高**
   ```yaml
   - alert: RedisConnectionFailureHigh
     expr: rate(redis_connection_errors_total[5m]) > 0.1
     for: 2m
     annotations:
       summary: "Redis connection failure rate is high"
   ```

3. **限流拒绝率异常**
   ```yaml
   - alert: RateLimitRejectRateHigh
     expr: rate(rate_limit_acquire_total{result="limit_exceeded"}[5m]) > 100
     for: 5m
     annotations:
       summary: "Rate limit reject rate is unusually high"
   ```
