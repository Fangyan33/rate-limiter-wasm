# Counter Service 服务端实现计划

## Context（背景）

当前 rate-limiter-wasm 插件已经支持分布式限流配置，但实际的 counter_service 服务端尚未实现。

**现状：**
- WASM 插件的客户端代码（`internal/store/client.go`）是占位符，返回 `ErrStoreUnavailable`，在 counter_service 模式下不会被调用，只是为未来可能的同步分布式后端预留接口。实际实现位置指向 internal/plugin/root.go 的异步流程。
- 设计文档已存在：`docs/plans/2026-03-10-http-counter-service-design.md` 定义了完整的 HTTP API 契约
- 配置模型已支持 `distributed_limit.counter_service.cluster`
- 缺少独立的服务端实现

**需求：**
实现一个独立的 counter_service HTTP 服务，提供原子性的并发计数器操作，使用 Redis 作为后端存储。

## 设计概述

基于现有设计文档 `docs/plans/2026-03-10-http-counter-service-design.md`，实现一个独立的 HTTP 服务：

**服务职责：**
- 提供 `POST /acquire` 和 `POST /release` HTTP 端点
- 使用 Redis 执行原子性的计数器操作
- 实现 counter + lease-TTL 模型防止计数器泄漏

**技术栈：**
- Go 1.22（与主项目保持一致）
- 标准库 `net/http` 或轻量级框架
- Redis 客户端：`github.com/redis/go-redis/v9`

**部署模型：**
- 独立的 Go 服务，可部署为 Kubernetes Deployment
- 通过 Envoy cluster 配置暴露给 WASM 插件

## 实现步骤

### 1. 项目结构设计

创建独立的 counter-service 目录结构：

```
cmd/counter-service/
  main.go                    # 服务入口
internal/counter-service/
  handler/
    acquire.go               # /acquire 端点处理
    release.go               # /release 端点处理
  redis/
    client.go                # Redis 客户端封装
    operations.go            # 原子操作实现
  models/
    types.go                 # 请求/响应类型定义
  config/
    config.go                # 服务配置
```

**关键文件：**
- `cmd/counter-service/main.go` - HTTP 服务器启动
- `internal/counter-service/redis/operations.go` - Redis 原子操作逻辑

### 2. Redis 数据模型实现

按照设计文档的 Key 模式：

**Counter Key:**
```
rl:concurrent:<api_key>:count
```
- 类型：整数
- 操作：INCR/DECR

**Lease Key:**
```
rl:concurrent:<api_key>:lease:<lease_id>
```
- 类型：字符串（存储时间戳或空值）
- TTL：从配置读取（默认 30000ms）

**Acquire 原子操作（Lua 脚本）：**
```lua
local counter_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])

local current = tonumber(redis.call('GET', counter_key) or 0)
if current >= limit then
  return {0, ""}  -- denied
end

redis.call('INCR', counter_key)
redis.call('SET', lease_key, '1', 'PX', ttl_ms)
return {1, lease_key}  -- allowed + lease_id
```

**Release 原子操作（Lua 脚本）：**
```lua
local counter_key = KEYS[1]
local lease_key = KEYS[2]

if redis.call('EXISTS', lease_key) == 1 then
  redis.call('DEL', lease_key)
  redis.call('DECR', counter_key)
  return 1  -- released
end
return 0  -- lease not found
```

### 3. HTTP API 实现

**`POST /acquire` 处理逻辑：**
1. 解析 JSON 请求体：`api_key`, `limit`, `ttl_ms`
2. 生成唯一 `lease_id`（UUID 或时间戳+随机数）
3. 执行 Redis Acquire Lua 脚本
4. 返回 JSON 响应：
   - 成功：`{"allowed": true, "lease_id": "..."}`
   - 拒绝：`{"allowed": false}`
   - 错误：HTTP 500

**`POST /release` 处理逻辑：**
1. 解析 JSON 请求体：`api_key`, `lease_id`
2. 执行 Redis Release Lua 脚本
3. 返回 JSON 响应：
   - `{"released": true}` 或 `{"released": false}`
   - 错误：HTTP 500

**健康检查端点：**
- `GET /health` - 返回 200 OK，可选检查 Redis 连接

### 4. 配置管理

服务配置通过环境变量或配置文件：

```yaml
server:
  port: 8080
  read_timeout: 5s
  write_timeout: 5s

redis:
  address: "localhost:6379"
  password: ""
  db: 0
  pool_size: 10
  max_retries: 3
```

### 5. 部署配置

创建 Kubernetes 部署文件：

**`deploy/istio/counter-service-deployment.yaml`：**
- Deployment：counter-service 容器
- Service：ClusterIP 类型，暴露 8080 端口
- ConfigMap：Redis 连接配置

**更新 `rate-limiter-envoyfilter.yaml`：**
- 添加 counter-service 的 Envoy cluster 定义
- 指向 counter-service.default.svc.cluster.local:8080

### 6. 测试策略

**单元测试：**
- `redis/operations_test.go`：测试 Lua 脚本逻辑
  - 并发限制正确执行
  - Lease TTL 过期后可重新获取
  - 重复 release 不会导致计数器错误

**集成测试：**
- `handler/acquire_test.go`：测试 HTTP 端点
  - 正常 acquire/release 流程
  - 达到限制时拒绝请求
  - 无效请求返回 400

**端到端测试：**
- 使用 Redis testcontainer
- 模拟多个并发请求
- 验证计数器准确性

### 7. 构建和部署

**构建脚本 `build-counter-service.sh`：**
```bash
#!/bin/bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o dist/counter-service \
  ./cmd/counter-service
```

**Docker 镜像：**
```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o counter-service ./cmd/counter-service

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/counter-service /counter-service
ENTRYPOINT ["/counter-service"]
```

## 验证计划

实现完成后的验证步骤：

1. **本地测试：**
   ```bash
   # 启动 Redis
   docker run -d -p 6379:6379 redis:7-alpine

   # 启动 counter-service
   go run ./cmd/counter-service

   # 测试 acquire
   curl -X POST http://localhost:8080/acquire \
     -H "Content-Type: application/json" \
     -d '{"api_key":"test","limit":5,"ttl_ms":30000}'

   # 测试 release
   curl -X POST http://localhost:8080/release \
     -H "Content-Type: application/json" \
     -d '{"api_key":"test","lease_id":"<lease_id>"}'
   ```

2. **单元测试：**
   ```bash
   go test ./internal/counter-service/... -count=1
   ```

3. **集成测试（Kubernetes）：**
   - 部署 counter-service 到集群
   - 更新 rate-limiter WASM 插件配置
   - 发送测试请求验证分布式限流生效

## 关键文件清单

**新增文件：**
- `cmd/counter-service/main.go`
- `internal/counter-service/handler/acquire.go`
- `internal/counter-service/handler/release.go`
- `internal/counter-service/redis/client.go`
- `internal/counter-service/redis/operations.go`
- `internal/counter-service/models/types.go`
- `internal/counter-service/config/config.go`
- `deploy/istio/counter-service-deployment.yaml`
- `build-counter-service.sh`
- `Dockerfile.counter-service`

**修改文件：**
- `deploy/istio/rate-limiter-envoyfilter.yaml` - 添加 counter-service cluster
- `go.mod` - 添加 Redis 客户端依赖

## 实现优先级

1. **Phase 1 - 核心功能（必需）：**
   - Redis 客户端和 Lua 脚本
   - /acquire 和 /release 端点
   - 基本配置管理

2. **Phase 2 - 生产就绪（重要）：**
   - 单元测试和集成测试
   - 健康检查端点
   - 错误处理和日志

3. **Phase 3 - 部署支持（必需）：**
   - Kubernetes 部署配置
   - Docker 镜像构建
   - Envoy cluster 配置

4. **Phase 4 - 优化（可选）：**
   - 指标暴露（Prometheus）
   - 连接池优化
   - 性能测试
