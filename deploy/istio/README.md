# Istio 部署指南

本目录包含在 Istio 环境中部署 rate-limiter WASM 插件的配置示例。

## 文件说明

- `rate-limiter-envoyfilter.yaml` - Istio EnvoyFilter 配置，用于加载和配置 WASM 插件
- `rate-limiter-plugin-config.yaml` - 插件配置示例（独立配置文件）
- `counter-service-deployment.yaml` - Counter Service 部署配置（用于分布式限流）

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

### 2. 分布式限流模式（Counter Service）

使用异步 HTTP 调用与 Counter Service 通信，实现跨多个 Envoy 实例的分布式限流。

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

#### Counter Service 配置说明

- `cluster`: Envoy 集群名称，指向 Counter Service
- `acquire_path`: 获取限流槽位的 API 路径
- `release_path`: 释放限流槽位的 API 路径
- `timeout_ms`: HTTP 调用超时时间（毫秒）
- `lease_ttl_ms`: 租约 TTL（毫秒），防止槽位泄漏

#### Counter Service API 契约

**Acquire 请求：**
```json
POST /acquire
Content-Type: application/json

{
  "api_key": "key_basic_001",
  "limit": 2,
  "ttl_ms": 30000
}
```

**Acquire 响应（成功）：**
```json
HTTP/1.1 200 OK
Content-Type: application/json

{
  "allowed": true,
  "lease_id": "lease-uuid-123"
}
```

**Acquire 响应（拒绝）：**
```json
HTTP/1.1 200 OK
Content-Type: application/json

{
  "allowed": false
}
```

**Release 请求：**
```json
POST /release
Content-Type: application/json

{
  "api_key": "key_basic_001",
  "lease_id": "lease-uuid-123"
}\n
Release 响应会被忽略（best-effort）。

## 部署步骤

### 前置条件

1. Kubernetes 集群已安装 Istio
2. 已构建 WASM 模块：`bash ./build.sh`
3. WASM 模块已上传到可访问的 HTTP 服务器

### 步骤 1：部署 Counter Service（可选）

如果使用分布式限流模式，需要先部署 Counter Service：

```bash
kubectl apply -f counter-service-deployment.yaml
```

确保 Counter Service 正常运行：
```bash
kubectl get pods -l app=ratelimit-service
kubectl logs -l app=ratelimit-service
```

### 步骤 2：更新 WASM 模块 SHA256

计算 WASM 模块的 SHA256：
```bash
sha256sum dist/rate-limiter.wasm
```

更新 `rate-limiter-envoyfilter.yaml` 中的 `sha256` 字段。

### 步骤 3：部署 EnvoyFilter

```bash
kubectl apply -f rate-limiter-envoyfilter.yaml
```

### 步骤 4：验证部署

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

## 降级行为

当 Counter Service 不可用时，插件会自动降级到本地限流模式：

1. HTTP 调用失败或超时 → 使用本地 limiter
2. Counter Service 返回非 200 状态 → 使用本地 limiter
3. 响应解析失败 → 使用本地 limiter

降级期间会记录警告日志，可通过 Envoy 日志查看：
```bash
kubectl logs <gateway-pod> -n istio-system | grep -i "falling back to local limiter"
```

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

- Counter Service 响应时间
- Counter Service 错误率
- 降级到本地限流的频率
- 429 响应数量
- 每个 API Key 的并发请求数

可以通过 Envoy 的统计信息获取这些指标：
```bash
kubectl exec <gateway-pod> -n istio-system -- \
  curl -s http://localhost:15000/stats | grep rate_limiter
```
