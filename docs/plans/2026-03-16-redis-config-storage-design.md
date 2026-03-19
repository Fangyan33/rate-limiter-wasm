# Redis 配置存储设计：基于 Domain+API Key 的动态限流配置

## 概述

本文档描述将 API Key 限流配置从 WASM 插件静态配置迁移到 Redis 动态存储的设计方案。支持按 `domain + api_key` 维度配置不同的并发限制，使用 Redis Hash 数据结构存储，通过 Lua 脚本实现配置读取与并发计数的原子操作。

## 背景与动机

### 当前设计的问题

当前限流配置硬编码在 WASM 插件的 `configuration` 中：

```yaml
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 2
  - api_key: key_premium_001
    max_concurrent: 5
```

当 API Key 数量超过 2000 个时，存在以下问题：

1. **配置体积过大**：EnvoyFilter configuration 可能达到几百 KB，影响 Istio 配置分发性能
2. **更新困难**：每次新增/修改 API Key 都需要重新部署 EnvoyFilter
3. **内存占用**：每个 Envoy sidecar 都保存完整配置副本
4. **扩展性差**：无法动态管理 API Key，运维成本高
5. **缺乏灵活性**：无法按 domain 维度差异化配置

### 设计目标

1. 支持 2000+ API Key 的动态管理
2. 支持 `domain + api_key` 维度的差异化限流配置
3. 配置变更实时生效，无需重启服务
4. 使用 Lua 脚本保证配置读取与计数操作的原子性
5. 复用现有 Redis 基础设施，不引入新依赖

---

## 数据模型设计

### Redis Key 命名规范

```
rl:config:<domain>:<api_key>     # 配置 Hash
rl:counter:<domain>:<api_key>   # 并发计数器
rl:lease:<lease_id>             # 租约 Key
```

### 配置存储结构（Hash）

使用 Redis Hash 存储每个 `domain + api_key` 组合的配置：

```
Key: rl:config:<domain>:<api_key>
Fields:
  - max_concurrent: int     # 最大并发数
  - enabled: bool           # 是否启用（"true"/"false"）
  - tier: string            # 可选，客户等级（"basic"/"premium"/"enterprise"）
  - description: string     # 可选，配置描述
  - updated_at: timestamp   # 可选，最后更新时间
```

### 示例数据

```bash
# api.example.com 域名下的配置
HSET rl:config:api.example.com:key_basic_001 \
    max_concurrent 2 \
    enabled true \
    tier basic

HSET rl:config:api.example.com:key_premium_001 \
    max_concurrent 10 \
    enabled true \
    tier premium

# api.internal.com 域名下的配置（同一 API Key 不同限制）
HSET rl:config:api.internal.com:key_premium_001 \
    max_concurrent 50 \
    enabled true \
    tier premium
```

### 通配符域名支持

为支持通配符域名匹配，配置查找顺序：

1. 精确匹配：`rl:config:<exact_domain>:<api_key>`
2. 通配符匹配：`rl:config:*.<parent_domain>:<api_key>`
3. 全局默认：`rl:config:*:<api_key>`

示例：
- 请求域名 `api.example.com`
- 查找顺序：
  1. `rl:config:api.example.com:key_001`
  2. `rl:config:*.example.com:key_001`
  3. `rl:config:*:key_001`

---

## Lua 脚本设计

### 脚本 1：Acquire（获取并发槽位）

将配置读取、计数检查、计数递增合并为原子操作：

```lua
-- acquire_with_config.lua
-- KEYS[1]: config_key (rl:config:<domain>:<api_key>)
-- KEYS[2]: counter_key (rl:counter:<domain>:<api_key>)
-- KEYS[3]: lease_key (rl:lease:<lease_id>)
-- KEYS[4]: wildcard_config_key (rl:config:*.<parent_domain>:<api_key>) 可选
-- KEYS[5]: global_config_key (rl:config:*:<api_key>) 可选
-- ARGV[1]: lease_ttl_ms
-- ARGV[2]: lease_id

local function get_config(key)
    if not key or key == "" then
        return nil
    end
    local config = redis.call('HGETALL', key)
    if #config == 0 then
        return nil
    end
    local result = {}
    for i = 1, #config, 2 do
        result[config[i]] = config[i + 1]
    end
    return result
end

-- 按优先级查找配置
local config = get_config(KEYS[1])
if not config then
    config = get_config(KEYS[4])
end
if not config then
    config = get_config(KEYS[5])
end

-- 配置不存在
if not config then
    return cjson.encode({
        allowed = false,
        reason = "config_not_found",
        message = "No rate limit configuration found for this domain and api_key"
    })
end

-- 检查是否启用
if config.enabled ~= "true" then
    return cjson.encode({
        allowed = false,
        reason = "api_key_disabled",
        message = "API key is disabled"
    })
end

local max_concurrent = tonumber(config.max_concurrent)
if not max_concurrent or max_concurrent <= 0 then
    return cjson.encode({
        allowed = false,
        reason = "invalid_config",
        message = "Invalid max_concurrent configuration"
    })
end

-- 获取当前计数
local counter_key = KEYS[2]
local current = tonumber(redis.call('GET', counter_key) or 0)

-- 检查是否超限
if current >= max_concurrent then
    return cjson.encode({
        allowed = false,
        reason = "limit_exceeded",
        max_concurrent = max_concurrent,
        current_count = current,
        message = "Concurrent limit exceeded"
    })
end

-- 递增计数器
local new_count = redis.call('INCR', counter_key)

-- 创建租约
local lease_key = KEYS[3]
local lease_ttl_ms = tonumber(ARGV[1])
local lease_id = ARGV[2]
redis.call('SET', lease_key, counter_key, 'PX', lease_ttl_ms)

return cjson.encode({
    allowed = true,
    lease_id = lease_id,
    max_concurrent = max_concurrent,
    current_count = new_count,
    tier = config.tier or "default"
})
```

### 脚本 2：Release（释放并发槽位）

```lua
-- release_with_lease.lua
-- KEYS[1]: lease_key (rl:lease:<lease_id>)
-- 无需 ARGV

-- 检查租约是否存在
local counter_key = redis.call('GET', KEYS[1])
if not counter_key then
    return cjson.encode({
        released = false,
        reason = "lease_not_found",
        message = "Lease not found or already expired"
    })
end

-- 删除租约
redis.call('DEL', KEYS[1])

-- 递减计数器
local new_count = redis.call('DECR', counter_key)

-- 防止计数器变为负数
if new_count < 0 then
    redis.call('SET', counter_key, 0)
    new_count = 0
end

return cjson.encode({
    released = true,
    current_count = new_count
})
```

### 脚本 3：批量查询配置（运维用）

```lua
-- list_configs.lua
-- KEYS[1]: pattern (rl:config:*)
-- ARGV[1]: cursor
-- ARGV[2]: count

local cursor = ARGV[1]
local count = tonumber(ARGV[2]) or 100

local result = redis.call('SCAN', cursor, 'MATCH', KEYS[1], 'COUNT', count)
local new_cursor = result[1]
local keys = result[2]

local configs = {}
for i, key in ipairs(keys) do
    local config = redis.call('HGETALL', key)
    if #config > 0 then
        local item = { key = key }
        for j = 1, #config, 2 do
            item[config[j]] = config[j + 1]
        end
        table.insert(configs, item)
    end
end

return cjson.encode({
    cursor = new_cursor,
    configs = configs
})
```

---

## API 设计变更

### Acquire API 变更

**请求（新增 domain 字段）：**

```json
POST /acquire
{
    "domain": "api.example.com",
    "api_key": "key_premium_001",
    "ttl_ms": 30000
}
```

**响应（成功）：**

```json
{
    "allowed": true,
    "lease_id": "550e8400-e29b-41d4-a716-446655440000",
    "max_concurrent": 10,
    "current_count": 3,
    "tier": "premium"
}
```

**响应（拒绝 - 超限）：**

```json
{
    "allowed": false,
    "reason": "limit_exceeded",
    "max_concurrent": 10,
    "current_count": 10,
    "message": "Concurrent limit exceeded"
}
```

**响应（拒绝 - 配置不存在）：**

```json
{
    "allowed": false,
    "reason": "config_not_found",
    "message": "No rate limit configuration found for this domain and api_key"
}
```

**响应（拒绝 - API Key 禁用）：**

```json
{
    "allowed": false,
    "reason": "api_key_disabled",
    "message": "API key is disabled"
}
```

### Release API（无变更）

```json
POST /release
{
    "api_key": "key_premium_001",
    "lease_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### 新增：配置管理 API

#### 创建/更新配置

```json
PUT /config
{
    "domain": "api.example.com",
    "api_key": "key_premium_001",
    "max_concurrent": 10,
    "enabled": true,
    "tier": "premium",
    "description": "Premium tier API key"
}
```

#### 查询配置

```json
GET /config?domain=api.example.com&api_key=key_premium_001
```

#### 删除配置

```json
DELETE /config?domain=api.example.com&api_key=key_premium_001
```

#### 批量查询配置

```json
GET /configs?domain=api.example.com&cursor=0&limit=100
```

---

## WASM 插件变更

### 配置模型变更

移除 `rate_limits` 配置项，改为可选：

```yaml
# 精简后的 WASM 插件配置
domains:
  - api.example.com
  - "*.internal.example.com"

distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: counter-service
    acquire_path: /acquire
    release_path: /release
    timeout_ms: 5000
    lease_ttl_ms: 30000

# rate_limits 移除，由 counter-service 从 Redis 读取

error_response:
  status_code: 429
  message: Rate limit exceeded

token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
```

### Acquire 请求变更

在 `OnHttpRequestHeaders` 中，增加 domain 参数：

```go
// 构造 acquire 请求
acquireReq := AcquireRequest{
    Domain: host,      // 新增：从 :authority 获取
    APIKey: apiKey,
    TTLMS:  cfg.DistributedLimit.CounterService.LeaseTTLMS,
}
```

### 配置验证变更

```go
func (c *Config) Validate() error {
    // ...

    // 当启用分布式限流时，rate_limits 可以为空
    // 配置由 counter-service 从 Redis 读取
    if !c.DistributedLimit.Enabled && len(c.RateLimits) == 0 {
        return fmt.Errorf("rate_limits required when distributed_limit is disabled")
    }

    // ...
}
```

---

## Counter-Service 实现变更

### 包结构

```
internal/counter-service/
  redis/
    client.go           # Redis 客户端封装
    operations.go       # Acquire/Release 操作
    scripts.go          # Lua 脚本定义
    config_ops.go       # 新增：配置管理操作
  handler/
    acquire.go          # Acquire HTTP handler
    release.go          # Release HTTP handler
    config.go           # 新增：配置管理 HTTP handler
  models/
    types.go            # 数据结构定义
```

### 核心实现

```go
// internal/counter-service/redis/operations.go

type AcquireRequest struct {
    Domain  string `json:"domain"`
    APIKey  string `json:"api_key"`
    TTLMS   int64  `json:"ttl_ms"`
}

type AcquireResult struct {
    Allowed       bool   `json:"allowed"`
    LeaseID       string `json:"lease_id,omitempty"`
    Reason        string `json:"reason,omitempty"`
    Message       string `json:"message,omitempty"`
    MaxConcurrent int    `json:"max_concurrent,omitempty"`
    CurrentCount  int    `json:"current_count,omitempty"`
    Tier          string `json:"tier,omitempty"`
}

func (c *Client) Acquire(ctx context.Context, req AcquireRequest) (*AcquireResult, error) {
    leaseID := uuid.New().String()

    // 构造 Keys
    configKey := fmt.Sprintf("rl:config:%s:%s", req.Domain, req.APIKey)
    counterKey := fmt.Sprintf("rl:counter:%s:%s", req.Domain, req.APIKey)
    leaseKey := fmt.Sprintf("rl:lease:%s", leaseID)

    // 通配符配置 Key
    wildcardConfigKey := ""
    if parts := strings.SplitN(req.Domain, ".", 2); len(parts) == 2 {
        wildcardConfigKey = fmt.Sprintf("rl:config:*.%s:%s", parts[1], req.APIKey)
    }
    globalConfigKey := fmt.Sprintf("rl:config:*:%s", req.APIKey)

    // 执行 Lua 脚本
    result, err := c.rdb.Eval(ctx, acquireWithConfigScript,
        []string{configKey, counterKey, leaseKey, wildcardConfigKey, globalConfigKey},
        req.TTLMS, leaseID,
    ).Result()

    if err != nil {
        return nil, fmt.Errorf("execute acquire script: %w", err)
    }

    // 解析 JSON 结果
    var acquireResult AcquireResult
    if err := json.Unmarshal([]byte(result.(string)), &acquireResult); err != nil {
        return nil, fmt.Errorf("parse acquire result: %w", err)
    }

    return &acquireResult, nil
}
```

### 配置管理实现

```go
// internal/counter-service/redis/config_ops.go

type RateLimitConfig struct {
    Domain        string `json:"domain"`
    APIKey        string `json:"api_key"`
    MaxConcurrent int    `json:"max_concurrent"`
    Enabled       bool   `json:"enabled"`
    Tier          string `json:"tier,omitempty"`
    Description   string `json:"description,omitempty"`
}

func (c *Client) SetConfig(ctx context.Context, cfg RateLimitConfig) error {
    key := fmt.Sprintf("rl:config:%s:%s", cfg.Domain, cfg.APIKey)

    fields := map[string]interface{}{
        "max_concurrent": cfg.MaxConcurrent,
        "enabled":        strconv.FormatBool(cfg.Enabled),
        "updated_at":     time.Now().Unix(),
    }

    if cfg.Tier != "" {
        fields["tier"] = cfg.Tier
    }
    if cfg.Description != "" {
        fields["description"] = cfg.Description
    }

    return c.rdb.HSet(ctx, key, fields).Err()
}

func (c *Client) GetConfig(ctx context.Context, domain, apiKey string) (*RateLimitConfig, error) {
    key := fmt.Sprintf("rl:config:%s:%s", domain, apiKey)

    result, err := c.rdb.HGetAll(ctx, key).Result()
    if err != nil {
        return nil, err
    }

    if len(result) == 0 {
        return nil, nil
    }

    maxConcurrent, _ := strconv.Atoi(result["max_concurrent"])
    enabled := result["enabled"] == "true"

    return &RateLimitConfig{
        Domain:        domain,
        APIKey:        apiKey,
        MaxConcurrent: maxConcurrent,
        Enabled:       enabled,
        Tier:          result["tier"],
        Description:   result["description"],
    }, nil
}

func (c *Client) DeleteConfig(ctx context.Context, domain, apiKey string) error {
    key := fmt.Sprintf("rl:config:%s:%s", domain, apiKey)
    return c.rdb.Del(ctx, key).Err()
}
```

---

## 数据迁移方案

### 从 YAML 配置迁移到 Redis

```bash
#!/bin/bash
# migrate_config.sh

# 从现有 YAML 配置读取并写入 Redis
DOMAIN="api.example.com"

# 示例：批量导入
cat <<EOF | while read api_key max_concurrent tier; do
    redis-cli HSET "rl:config:${DOMAIN}:${api_key}" \
        max_concurrent "${max_concurrent}" \
        enabled "true" \
        tier "${tier}" \
        updated_at "$(date +%s)"
done
key_basic_001 2 basic
key_basic_002 2 basic
key_premium_001 10 premium
key_premium_002 10 premium
key_enterprise_001 50 enterprise
EOF
```

### 配置导出脚本

```bash
#!/bin/bash
# export_config.sh

# 导出所有配置到 JSON
redis-cli --scan --pattern "rl:config:*" | while read key; do
    echo -n "{\"key\":\"$key\","
    redis-cli HGETALL "$key" | paste - - | \
        awk '{printf "\"%s\":\"%s\",", $1, $2}' | \
        sed 's/,$//'
    echo "}"
done | jq -s '.'
```

---

## 测试策略

### 单元测试

```go
// internal/counter-service/redis/operations_test.go

func TestAcquireWithConfig(t *testing.T) {
    tests := []struct {
        name           string
        setupConfig    func(mr *miniredis.Miniredis)
        request        AcquireRequest
        expectedResult AcquireResult
    }{
        {
            name: "successful acquire with exact domain match",
            setupConfig: func(mr *miniredis.Miniredis) {
                mr.HSet("rl:config:api.example.com:key_001",
                    "max_concurrent", "5",
                    "enabled", "true",
                    "tier", "premium",
                )
            },
            request: AcquireRequest{
                Domain: "api.example.com",
                APIKey: "key_001",
                TTLMS:  30000,
            },
            expectedResult: AcquireResult{
                Allowed:       true,
                MaxConcurrent: 5,
                CurrentCount:  1,
                Tier:          "premium",
            },
        },
        {
            name: "fallback to wildcard config",
            setupConfig: func(mr *miniredis.Miniredis) {
                mr.HSet("rl:config:*.example.com:key_001",
                    "max_concurrent", "3",
                    "enabled", "true",
                )
            },
            request: AcquireRequest{
                Domain: "api.example.com",
                APIKey: "key_001",
                TTLMS:  30000,
            },
            expectedResult: AcquireResult{
                Allowed:       true,
                MaxConcurrent: 3,
                CurrentCount:  1,
            },
        },
        {
            name: "reject when config not found",
            setupConfig: func(mr *miniredis.Miniredis) {
                // no config
            },
            request: AcquireRequest{
                Domain: "api.example.com",
                APIKey: "unknown_key",
                TTLMS:  30000,
            },
            expectedResult: AcquireResult{
                Allowed: false,
                Reason:  "config_not_found",
            },
        },
        {
            name: "reject when api_key disabled",
            setupConfig: func(mr *miniredis.Miniredis) {
                mr.HSet("rl:config:api.example.com:key_001",
                    "max_concurrent", "5",
                    "enabled", "false",
                )
            },
            request: AcquireRequest{
                Domain: "api.example.com",
                APIKey: "key_001",
                TTLMS:  30000,
            },
            expectedResult: AcquireResult{
                Allowed: false,
                Reason:  "api_key_disabled",
            },
        },
        {
            name: "reject when limit exceeded",
            setupConfig: func(mr *miniredis.Miniredis) {
                mr.HSet("rl:config:api.example.com:key_001",
                    "max_concurrent", "2",
                    "enabled", "true",
                )
                mr.Set("rl:counter:api.example.com:key_001", "2")
            },
            request: AcquireRequest{
                Domain: "api.example.com",
                APIKey: "key_001",
                TTLMS:  30000,
            },
            expectedResult: AcquireResult{
                Allowed:       false,
                Reason:        "limit_exceeded",
                MaxConcurrent: 2,
                CurrentCount:  2,
            },
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            mr := miniredis.RunT(t)
            tt.setupConfig(mr)

            client := NewClient(mr.Addr(), "", 0)
            result, err := client.Acquire(context.Background(), tt.request)

            require.NoError(t, err)
            assert.Equal(t, tt.expectedResult.Allowed, result.Allowed)
            assert.Equal(t, tt.expectedResult.Reason, result.Reason)
            // ... more assertions
        })
    }
}
```

### 集成测试

```go
func TestDomainPriorityMatching(t *testing.T) {
    // 测试配置查找优先级：精确匹配 > 通配符 > 全局
}

func TestConcurrentAcquireAtomicity(t *testing.T) {
    // 测试并发场景下的原子性
}

func TestLeaseExpiration(t *testing.T) {
    // 测试租约过期后的行为
}
```

---

## 部署与运维

### Redis 内存估算

```
每个配置项：
- Key: ~50 bytes (rl:config:api.example.com:key_premium_001)
- Hash fields: ~100 bytes (max_concurrent, enabled, tier, updated_at)
- 总计: ~150 bytes/配置

2000 个 API Key × 5 个域名 = 10,000 个配置
内存占用: 10,000 × 150 bytes ≈ 1.5 MB

计数器和租约（峰值）:
- 10,000 个计数器 × 20 bytes ≈ 200 KB
- 50,000 个活跃租约 × 100 bytes ≈ 5 MB

总计: < 10 MB
```

### 监控指标

```
# 配置相关
rate_limit_config_total{domain="api.example.com"}  # 配置数量
rate_limit_config_lookups_total{result="hit|miss|wildcard"}  # 配置查找统计

# 限流相关
rate_limit_acquire_total{domain="...", result="allowed|denied|error"}
rate_limit_release_total{domain="...", result="success|not_found"}
rate_limit_current_concurrent{domain="...", api_key="..."}
```

### 告警规则

```yaml
groups:
  - name: rate-limiter-config
    rules:
      - alert: ConfigLookupMissRate
        expr: |
          rate(rate_limit_config_lookups_total{result="miss"}[5m]) /
          rate(rate_limit_config_lookups_total[5m]) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High config lookup miss rate"

      - alert: RedisLatencyHigh
        expr: |
          histogram_quantile(0.99, rate(redis_command_duration_seconds_bucket[5m])) > 0.01
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Redis P99 latency > 10ms"
```

---

## 实现计划

### 阶段 1：Counter-Service 变更（2-3 天）

1. 实现 Lua 脚本（acquire_with_config, release_with_lease）
2. 修改 Acquire API 支持 domain 参数
3. 实现配置��理 API（CRUD）
4. 单元测试

### 阶段 2：WASM 插件变更（1-2 天）

1. 修改 Acquire 请求增加 domain 字段
2. 修改配置验证逻辑（rate_limits 可选）
3. 更新部署示例配置
4. 集成测试

### 阶段 3：数据迁移与部署（1 天）

1. 编写配置迁移脚本
2. 迁移现有配置到 Redis
3. 灰度发布验证
4. 文档更新

---

## 风险与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| Redis 不可用 | 所有请求无法获取配置 | 保留本地 fallback 机制 |
| 配置查找延迟 | 请求延迟增加 | Lua 脚本原子操作，减少网络往返 |
| 配置数据丢失 | 限流失效 | Redis 持久化 + 定期备份 |
| 通配符匹配性能 | 查找变慢 | 限制通配符层级，优先精确匹配 |

---

## 附录：完整 Lua 脚本

### acquire_with_config.lua

```lua
-- 完整版本见上文 "脚本 1：Acquire" 部分
```

### release_with_lease.lua

```lua
-- 完整版本见上文 "脚本 2：Release" 部分
```
