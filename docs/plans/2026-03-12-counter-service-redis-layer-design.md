# Counter Service - Redis 操作层设计

> 状态：历史记录，已由[当前限流契约](../specs/rate-limiting-contract.md)替代。
>
> 当前使用哈希配置键、ZSET租约集合；API Key 可为空。环境变量为 REDIS_ADDR、REDIS_PASSWORD、REDIS_KEY_PREFIX、PORT，DB固定0。原连接配置和独立counter示例已替代。

## 当前行为修正

- 具体 key 的精确、直接父域、全局规则优先，再按相同顺序匹配空 key 的域名规则；enabled=false 逐条跳过，最终无规则直接放行。
- 只有命中启用规则后超限才拒绝；dispatch、Redis 或配置损坏均 fail-open，不转本地降级。损坏候选不回退。
- Redis 配置键为 domain + SHA256(api_key)，并发占用为实际域名 + 选中key摘要的 ZSET；空 key 也有固定摘要。
- 配置写入必须有布尔 enabled；如携带 api_key_hash 必须与原始key一致。查询对象没有 api_key，tier已删除，description省略保留、空串清空，域名统一小写。
- token-stats 已废弃；当前限流验收使用 --rate-limiter-only，不依赖统计或旧指标提取配置。

<details>
<summary>原始历史设计与实施片段（已替代，不可作为当前指令执行）</summary>

## 概述

本文档描述 counter-service 的核心 Redis 操作层设计，包括客户端封装、Lua 脚本、原子操作接口和数据模型。

## 设计决策

1. **Lease ID 生成**：使用 UUID v4，保证全局唯一性
2. **错误处理**：区分网络/超时错误（503）和其他错误（500）
3. **配置管理**：纯环境变量配置，符合 12-factor app
4. **Lua 脚本管理**：嵌入在 Go 代码中，简化部署

## 包结构

```
internal/counter-service/
  redis/
    client.go      # Redis 客户端封装和连接管理
    operations.go  # Acquire/Release 原子操作实现
    scripts.go     # Lua 脚本常量定义
  models/
    types.go       # 请求/响应数据结构
```

## Redis 客户端封装 (client.go)

### Client 结构

```go
type Client struct {
    rdb *redis.Client
}

func NewClient(addr, password string, db int) (*Client, error)
func (c *Client) Ping(ctx context.Context) error
func (c *Client) Close() error
```

### 环境变量配置

- `REDIS_ADDRESS` (默认: `localhost:6379`)
- `REDIS_PASSWORD` (默认: 空)
- `REDIS_DB` (默认: `0`)
- `REDIS_POOL_SIZE` (默认: `10`)
- `REDIS_MAX_RETRIES` (默认: `3`)

### 错误分类

- **网络错误/超时** → 返回特定错误类型，HTTP 层映射为 503 Service Unavailable
- **其他错误** → 返回通用错误，HTTP 层映射为 500 Internal Server Error

## Lua 脚本定义 (scripts.go)

### Acquire 脚本

```lua
local counter_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])

local current = tonumber(redis.call('GET', counter_key) or 0)
if current >= limit then
  return {0, ""}
end

redis.call('INCR', counter_key)
redis.call('SET', lease_key, '1', 'PX', ttl_ms)
return {1, lease_key}
```

**返回值：**
- `{1, lease_key}` - 允许请求，返回 lease_key
- `{0, ""}` - 拒绝请求，已达限制

### Release 脚本

```lua
local counter_key = KEYS[1]
local lease_key = KEYS[2]

if redis.call('EXISTS', lease_key) == 1 then
  redis.call('DEL', lease_key)
  redis.call('DECR', counter_key)
  return 1
end
return 0
```

**返回值：**
- `1` - 成功释放
- `0` - lease 不存在（可能已过期）

### Key 生成规则

- **Counter Key**: `rl:concurrent:<api_key>:count`
- **Lease Key**: `rl:concurrent:<api_key>:lease:<lease_id>`

## 原子操作实现 (operations.go)

### Acquire 操作

```go
type AcquireRequest struct {
    APIKey string
    Limit  int64
    TTLMS  int64
}

type AcquireResult struct {
    Allowed bool
    LeaseID string
}

func (c *Client) Acquire(ctx context.Context, req AcquireRequest) (*AcquireResult, error)
```

**实现逻辑：**
1. 生成 UUID v4 作为 lease_id
2. 构造 counter_key 和 lease_key
3. 执行 Lua 脚本：`EVAL script 2 counter_key lease_key limit ttl_ms`
4. 解析返回值：`[allowed, lease_id]`
5. 错误处理：网络/超时错误包装为特定类型

### Release 操作

```go
type ReleaseRequest struct {
    APIKey  string
    LeaseID string
}

type ReleaseResult struct {
    Released bool
}

func (c *Client) Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error)
```

**实现逻辑：**
1. 构造 counter_key 和 lease_key
2. 执行 Lua 脚本：`EVAL script 2 counter_key lease_key`
3. 返回值：1 表示成功释放，0 表示 lease 不存在
4. 同样的错误分类处理

## 数据模型 (models/types.go)

### HTTP 请求/响应结构

```go
// Acquire endpoint
type AcquireRequest struct {
    APIKey string `json:"api_key"`
    Limit  int64  `json:"limit"`
    TTLMS  int64  `json:"ttl_ms"`
}

type AcquireResponse struct {
    Allowed bool   `json:"allowed"`
    LeaseID string `json:"lease_id,omitempty"`
}

// Release endpoint
type ReleaseRequest struct {
    APIKey  string `json:"api_key"`
    LeaseID string `json:"lease_id"`
}

type ReleaseResponse struct {
    Released bool `json:"released"`
}

// Error response
type ErrorResponse struct {
    Error string `json:"error"`
}
```

### 验证规则

- `api_key` 不能为空
- `limit` 必须 > 0
- `ttl_ms` 必须 > 0
- `lease_id` 不能为空（Release 时）

## 单元测试策略

使用 `miniredis` 进行单元测试，无需真实 Redis：

**测试场景：**
- 正常 acquire/release 流程
- 达到限制时拒绝请求
- Lease TTL 过期后可重新获取
- 重复 release 不会导致计数器错误
- 并发场景的原子性验证

**测试文件：**
- `internal/counter-service/redis/operations_test.go`
- `internal/counter-service/redis/client_test.go`

## 依赖

```go
require (
    github.com/redis/go-redis/v9 v9.x.x
    github.com/google/uuid v1.x.x
    github.com/alicebob/miniredis/v2 v2.x.x // 测试依赖
)
```

## 下一步

完成 Redis 操作层后，将实现：
1. HTTP API 层 (handler/)
2. 服务入口 (cmd/counter-service/main.go)
3. 部署配置 (Kubernetes manifests)

</details>
