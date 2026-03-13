# Lease Collection Model Design

**Date:** 2026-03-13
**Status:** Proposed
**Author:** System Design

## 1. 背景和问题陈述

### 1.1 当前架构

Counter-service 使用 Redis 实现分布式并发限流，核心数据结构：

```
{api_key}:count         → String (整数计数器)
{api_key}:lease:{uuid}  → String (值="1", TTL=lease_ttl_ms)
```

**Acquire 流程**：
1. 检查 `{api_key}:count` 是否 >= limit
2. 如果未满：INCR counter，SET lease key (带 TTL)
3. 返回 lease_id

**Release 流程**：
1. 检查 lease key 是否存在（作为"释放凭证"）
2. 如果存在：DEL lease key，DECR counter
3. 返回成功/失败

### 1.2 核心问题

**问题 1：TTL 过期导致 counter 泄漏**

当 SSE/长连接持续时间 > lease_ttl_ms 时：
- Lease key 在 TTL 到期后自动删除
- Counter 不会自动递减（设计如此）
- 客户端取消连接时，release 操作因 lease key 不存在而失败
- 结果：counter 永久泄漏，导致过度限流

**实际场景**：
- 默认 lease_ttl_ms = 30000 (30秒)
- SSE 连接可能持续数分钟到数小时
- 每个超时的连接都会泄漏一个 counter 槽位

**问题 2：Redis 驱逐导致 underflow 风险**

边界情况：
- Lease key 存在，但 counter_key 被 Redis 驱逐（内存压力）
- Release 时：DECR 一个不存在的 key → 创建值为 -1
- 结果：counter underflow，允许过多请求

### 1.3 为什么当前设计有结构性矛盾

Counter 和 lease 是两个独立的数据结构：
- Counter 通过 INCR/DECR 手动维护
- Lease 通过 TTL 自动过期
- **矛盾**：lease 过期不会触发 counter 递减，导致两者不一致

这是结构性问题，不是实现 bug。

## 2. 方案对比

### 2.1 方案 A：KeepAlive/续租

**核心思路**：
- 保持当前架构
- Proxy-WASM 插件定期发送 /renew 请求刷新 lease TTL
- 确保长连接的 lease 不会过期

**优点**：
- 改动最小，只需添加 renew 逻辑
- 保持现有 acquire/release 语义

**缺点**：
- 增加 QPS 负载：N 个活跃连接 / renew_interval = 额外 QPS
- Renew 失败仍可能导致泄漏
- 需要 Proxy-WASM 插件维护活跃 lease 注册表
- 复杂度：插件需要 OnTick 定时器 + 异步 HTTP 调用

**适用场景**：
- 并发连接数较少（< 1000）
- 可接受额外 QPS 开销
- 希望最小化架构变更

### 2.2 方案 B：大 TTL

**核心思路**：
- 将 lease_ttl_ms 设置为覆盖最长连接时间（如 24 小时）
- TTL 仅作为"崩溃恢复"的兜底机制

**优点**：
- 零代码改动
- 简单直接

**缺点**：
- 插件崩溃后，泄漏的 counter 需要 24 小时才能恢复
- 不适合需要快速恢复的场景
- 仍然有泄漏风险（连接 > 24 小时）

**适用场景**：
- 连接时长有明确上限
- 可接受长时间的崩溃恢复窗口
- 追求实现简单性

### 2.3 方案 C：租约集合模型（推荐）

**核心思路**：
- Counter 不再是独立的整数
- 使用 Redis ZSET 存储所有活跃 lease
- 当前占用数 = ZSET 大小（自动排除过期 lease）

**优点**：
- ✅ 结构性避免"lease 过期但 counter 没减"的矛盾
- ✅ Counter 从 lease 集合派生，不是独立维护
- ✅ 避免 counter_key 丢失导致的 underflow
- ✅ 保留 lease 作为"释放凭证"的幂等性

**缺点**：
- ⚠️ 实现复杂度更高（Lua 脚本更复杂）
- ⚠️ 性能从 O(1) 降到 O(log(N))（实际影响很小）
- ⚠️ 需要仔细测试边界情况

**适用场景**：
- 需要绝对准确的并发限制
- 长连接场景（SSE、WebSocket）
- 并发限制数量不是特别大（< 1000）
- 追求架构正确性

### 2.4 方案选择

**选择方案 C**，理由：
1. 从根本上解决结构性矛盾，而非打补丁
2. 性能影响可接受（log(100) ≈ 7 次比较）
3. 避免 KeepAlive 的额外 QPS 开销
4. 避免大 TTL 的长时间恢复窗口
5. 项目未上线，可以直接实施新架构

## 3. 详细设计

### 3.1 数据结构变更

**新数据结构**：

```
{api_key}:leases           → ZSET
                             - score: 过期时间戳（毫秒）
                             - member: lease_id (UUID)

{api_key}:lease:{uuid}     → String (值="1", TTL=lease_ttl_ms)
```

**为什么使用 ZSET + 独立 lease key 的混合方案**：

1. **ZSET 提供高效的过期清理**：
   - `ZREMRANGEBYSCORE key -inf now_ms` 删除所有过期项
   - 复杂度：O(log(N) + M)，N=总数，M=删除数
   - `ZCARD key` 获取当前大小，复杂度 O(1)

2. **独立 lease key 提供兜底机制**：
   - Redis TTL 自动清理，防止 ZSET 无限增长
   - 保留"释放凭证"语义，确保幂等性
   - 即使 ZSET 清理失败，lease key 最终会过期

3. **两者互补**：
   - 正常情况：acquire 时主动清理 ZSET
   - 异常情况：lease key TTL 兜底清理
   - Release 时：lease key 作为"凭证"，同时清理 ZSET

### 3.2 Acquire Lua 脚本设计

**输入参数**：

```lua
KEYS[1] = {api_key}:leases           -- ZSET
KEYS[2] = {api_key}:lease:{uuid}     -- String key
ARGV[1] = limit                      -- 整数
ARGV[2] = ttl_ms                     -- 整数
ARGV[3] = now_ms                     -- 当前时间戳（毫秒）
ARGV[4] = lease_id                   -- UUID
```

**脚本逻辑**：

```lua
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local lease_id = ARGV[4]

-- 1. 清理所有已过期的 lease（score < now_ms）
local removed = redis.call('ZREMRANGEBYSCORE', leases_key, '-inf', now_ms)

-- 2. 获取当前活跃 lease 数量
local current = redis.call('ZCARD', leases_key)

-- 3. 检查是否达到限制
if current >= limit then
  return {0, ""}
end

-- 4. 计算新 lease 的过期时间戳
local expire_at = now_ms + ttl_ms

-- 5. 添加到 ZSET
redis.call('ZADD', leases_key, expire_at, lease_id)

-- 6. 创建独立 lease key（带 TTL）
redis.call('SET', lease_key, '1', 'PX', ttl_ms)

-- 7. 返回成功 + lease_id
return {1, lease_id}
```

**关键点**：
- 每次 acquire 都先清理过期项，确保计数准确
- ZREMRANGEBYSCORE 是原子操作，不会有竞态条件
- Score 使用绝对时间戳，便于范围查询
- 同时创建 ZSET member 和独立 key，确保一致性

### 3.3 Release Lua 脚本设计

**输入参数**：

```lua
KEYS[1] = {api_key}:leases           -- ZSET
KEYS[2] = {api_key}:lease:{uuid}     -- String key
ARGV[1] = lease_id                   -- UUID
```

**脚本逻辑**：

```lua
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local lease_id = ARGV[1]

-- 1. 检查 lease key 是否存在（作为"释放凭证"）
if redis.call('EXISTS', lease_key) == 0 then
  return 0  -- 已释放或已过期
end

-- 2. 删除 lease key
redis.call('DEL', lease_key)

-- 3. 从 ZSET 中移除该 lease
redis.call('ZREM', leases_key, lease_id)

-- 4. 返回成功
return 1
```

**关键点**：
- 仍然用 lease key 存在性作为"凭证"，保持幂等性
- 同时清理 ZSET 和 lease key
- 如果 lease key 已过期但 ZSET 中仍有记录，ZREM 是无害的
- 下次 acquire 时会通过 ZREMRANGEBYSCORE 清理

## 4. 代码变更清单

### 4.1 文件：`internal/counter-service/redis/scripts.go`

**当前代码**（第 3-30 行）：

```go
const acquireScript = `
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
`

const releaseScript = `
local counter_key = KEYS[1]
local lease_key = KEYS[2]

if redis.call('EXISTS', lease_key) == 1 then
  redis.call('DEL', lease_key)
  redis.call('DECR', counter_key)
  return 1
end
return 0
`
```

**改动点**：

1. **重写 `acquireScript`**：
   - 移除 `counter_key` 参数
   - 添加 `now_ms` 和 `lease_id` 参数
   - 使用 ZSET 操作替代 INCR
   - 添加 ZREMRANGEBYSCORE 清理逻辑

2. **重写 `releaseScript`**：
   - 移除 `counter_key` 参数
   - 添加 ZREM 操作清理 ZSET
   - 保持 lease key 检查逻辑（幂等性）

**新代码**：

```go
const acquireScript = `
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local lease_id = ARGV[4]

redis.call('ZREMRANGEBYSCORE', leases_key, '-inf', now_ms)
local current = redis.call('ZCARD', leases_key)
if current >= limit then
  return {0, ""}
end

local expire_at = now_ms + ttl_ms
redis.call('ZADD', leases_key, expire_at, lease_id)
redis.call('SET', lease_key, '1', 'PX', ttl_ms)
return {1, lease_id}
`

const releaseScript = `
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local lease_id = ARGV[1]

if redis.call('EXISTS', lease_key) == 0 then
  return 0
end

redis.call('DEL', lease_key)
redis.call('ZREM', leases_key, lease_id)
return 1
`
```

### 4.2 文件：`internal/counter-service/redis/operations.go`

**当前代码**（第 45-75 行，Acquire 方法）：

```go
func (r *RedisOperations) Acquire(ctx context.Context, apiKey string, limit int, ttlMs int) (*AcquireResult, error) {
	leaseID := uuid.New().String()
	counterKey := fmt.Sprintf("%s:count", apiKey)
	leaseKey := fmt.Sprintf("%s:lease:%s", apiKey, leaseID)

	result, err := r.acquireScript.Run(ctx, r.client, []string{counterKey, leaseKey}, limit, ttlMs).Result()
	// ...
}
```

**改动点**：

1. **修改 key 命名**：
   - 移除 `counterKey`
   - 添加 `leasesKey`（ZSET）

2. **添加当前时间戳参数**：
   - `nowMs := time.Now().UnixMilli()`

3. **调整脚本调用参数**：
   - 传递 4 个 ARGV：limit, ttlMs, nowMs, leaseID

**新代码**：

```go
func (r *RedisOperations) Acquire(ctx context.Context, apiKey string, limit int, ttlMs int) (*AcquireResult, error) {
	leaseID := uuid.New().String()
	leasesKey := fmt.Sprintf("%s:leases", apiKey)
	leaseKey := fmt.Sprintf("%s:lease:%s", apiKey, leaseID)
	nowMs := time.Now().UnixMilli()

	result, err := r.acquireScript.Run(ctx, r.client, []string{leasesKey, leaseKey}, limit, ttlMs, nowMs, leaseID).Result()
	// ... 其余逻辑不变
}
```

**当前代码**（第 77-100 行，Release 方法）：

```go
func (r *RedisOperations) Release(ctx context.Context, apiKey string, leaseID string) (*ReleaseResult, error) {
	counterKey := fmt.Sprintf("%s:count", apiKey)
	leaseKey := fmt.Sprintf("%s:lease:%s", apiKey, leaseID)

	result, err := r.releaseScript.Run(ctx, r.client, []string{counterKey, leaseKey}).Result()
	// ...
}
```

**改动点**：

1. **修改 key 命名**：
   - 移除 `counterKey`
   - 添加 `leasesKey`

2. **添加 lease_id 参数**：
   - 传递给 Lua 脚本用于 ZREM

**新代码**：

```go
func (r *RedisOperations) Release(ctx context.Context, apiKey string, leaseID string) (*ReleaseResult, error) {
	leasesKey := fmt.Sprintf("%s:leases", apiKey)
	leaseKey := fmt.Sprintf("%s:lease:%s", apiKey, leaseID)

	result, err := r.releaseScript.Run(ctx, r.client, []string{leasesKey, leaseKey}, leaseID).Result()
	// ... 其余逻辑不变
}
```
