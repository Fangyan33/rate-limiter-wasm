# Counter-Service ZSET 租约集合迁移 Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 counter-service 的分布式并发占用模型从旧的 `counter + lease pointer` 迁移到 ZSET 租约集合模型，结构性修复 lease TTL 过期后槽位泄漏导致持续 429 的问题。

**Architecture:** 用 `rl:leases:<domain>:<api_key>` ZSET（score=过期时间戳）替代独立 counter；每次 acquire 前原子清理过期成员，并发数直接来自 ZCARD；`rl:lease:<lease_id>` 改为存储 leases_key 指针，仍作为 release 幂等凭证。配置结构、wildcard fallback、插件调用契约全部保持不变。

**Tech Stack:** Go 1.22、Redis、Lua（EVAL）、miniredis（测试）、go-redis

---

## File Structure

### 修改文件
- `internal/counter-service/redis/scripts.go`
  - 重写 `acquireScript`：counter 模型 → ZSET 模型
  - 重写 `releaseScript`：`DECR` → `ZREM + ZCARD`
  - `listConfigsScript` 保持不变
- `internal/counter-service/redis/operations.go`
  - `Acquire`：`counterKey` → `leasesKey`，新增 `now_ms`，调整 KEYS/ARGV 顺序
  - `Release`：新增 `ARGV[1]=lease_id`，解析新返回结构
- `internal/counter-service/redis/operations_test.go`
  - 更新断言：`rl:lease:<id>` 的 value 从 `rl:counter:...` 改为 `rl:leases:...`
  - 新增核心回归测试：lease 过期后 acquire 自动恢复
  - 新增 wildcard/global 回归断言：即使命中 fallback，并发集合 key 仍按实际请求 domain 构造
- `internal/counter-service/handler/acquire_test.go`
  - 补充 handler 层 lease 过期后再次 acquire 自动恢复测试
- `internal/counter-service/handler/release_test.go`
  - 明确对外契约：lease 不存在时仍返回 `200 + released=false + reason=lease_not_found`
- `internal/counter-service/handler/release.go`
  - 将 `redis.ErrLeaseNotFound` 映射为 `200 + released=false + reason=lease_not_found`

### 不改文件
- `internal/counter-service/handler/acquire.go` — 接口契约不变
- `internal/plugin/root.go` — 插件调用路径不变
- `internal/counter-service/redis/client.go` — 连接池不变

---

## Chunk 1: 重写 Lua 脚本（scripts.go）

### Task 1: 先写失败测试——验证 ZSET 数据结构

**Files:**
- Modify: `internal/counter-service/redis/operations_test.go`
- Reference: `internal/counter-service/redis/scripts.go`
- Reference: `internal/counter-service/redis/operations.go`

- [ ] **Step 1: 在 `TestAcquire_ExactMatch` 中增加 ZSET 断言（当前会失败）**

在现有 `TestAcquire_ExactMatch` 测试的断言区域，把旧的 counter/lease 断言替换为：

```go
// 检查 ZSET 成员存在
members, err := s.ZMembers("rl:leases:api.example.com:key001")
assert.NoError(t, err)
assert.Len(t, members, 1)
assert.Equal(t, result.LeaseID, members[0])

// 检查 lease key 存储的是 leases_key（不再是 counter_key）
leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
assert.Equal(t, "rl:leases:api.example.com:key001", leaseVal)
```

同时删除旧断言：
```go
// 删除这两行旧断言：
// count, _ := s.Get("rl:counter:api.example.com:key001")
// assert.Equal(t, "1", count)
// leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
// assert.Equal(t, "rl:counter:api.example.com:key001", leaseVal)
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/counter-service/redis -run TestAcquire_ExactMatch -count=1`
Expected: FAIL，当前脚本仍写 counter key，ZSET 断言找不到成员。

- [ ] **Step 3: 新增 lease 过期后自动恢复测试（核心回归）**

先在 `operations_test.go` 的 import 中增加：

```go
import (
    "context"
    "testing"
    "time"

    "github.com/alicebob/miniredis/v2"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"

    "rate-limiter-wasm/internal/counter-service/models"
    "rate-limiter-wasm/internal/counter-service/redis"
)
```

然后在 `operations_test.go` 末尾新增：

```go
func TestAcquire_LeaseExpiredAutoRecovery(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "1")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	ctx := context.Background()

	// 第一次 acquire，TTL 极短（100ms）
	r1, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  100,
	})
	require.NoError(t, err)
	require.True(t, r1.Allowed)

	// 模拟 lease 过期：推进 miniredis 时钟
	s.FastForward(200 * time.Millisecond)

	// 第二次 acquire 应该成功（过期成员被清理）
	r2, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})
	assert.NoError(t, err)
	assert.True(t, r2.Allowed, "acquire should succeed after lease expiry")
	assert.Equal(t, 1, r2.CurrentCount)
}
```

- [ ] **Step 4: 运行新测试确认失败**

Run: `go test ./internal/counter-service/redis -run TestAcquire_LeaseExpiredAutoRecovery -count=1`
Expected: FAIL，旧模型 counter 不会自动清理，第二次 acquire 被拒绝。

- [ ] **Step 5: 在 wildcard/global fallback 测试中补充 leases_key 断言**

在现有 `TestAcquire_WildcardFallback` 与 `TestAcquire_GlobalFallback` 中追加：

```go
leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
assert.Equal(t, "rl:leases:api.example.com:key001", leaseVal)
```

以及：

```go
leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
assert.Equal(t, "rl:leases:any.domain.com:key001", leaseVal)
```

目的：即使配置命中 wildcard / global，真实并发集合 key 仍按**实际请求 domain + api_key** 构造，而不是按配置 key 构造。

- [ ] **Step 6: 运行 fallback 测试确认当前实现部分失败**

Run: `go test ./internal/counter-service/redis -run 'TestAcquire_(WildcardFallback|GlobalFallback)' -count=1`
Expected: FAIL，当前 lease value 仍是 `rl:counter:...`，且不满足新断言。

- [ ] **Step 7: 新增 release 幂等测试**

```go
func TestRelease_Idempotent(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "5")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	ctx := context.Background()
	acqResult, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})
	require.NoError(t, err)

	// 第一次 release 成功
	r1, err := client.Release(ctx, acqResult.LeaseID)
	assert.NoError(t, err)
	assert.True(t, r1.Released)
	assert.Equal(t, 0, r1.CurrentCount)

	// 第二次 release 返回结构化 lease_not_found 结果，并伴随 ErrLeaseNotFound
	r2, err := client.Release(ctx, acqResult.LeaseID)
	assert.ErrorIs(t, err, redis.ErrLeaseNotFound)
	require.NotNil(t, r2)
	assert.False(t, r2.Released)
	assert.Equal(t, "lease_not_found", r2.Reason)
}
```

- [ ] **Step 8: 运行幂等测试确认当前行为（可能已通过，记录结果）**

Run: `go test ./internal/counter-service/redis -run TestRelease_Idempotent -count=1`
Expected: 可能 PASS（旧模型幂等性已有），记录结果即可。

---

### Task 2: 重写 acquireScript（ZSET 模型）

**Files:**
- Modify: `internal/counter-service/redis/scripts.go`

- [ ] **Step 1: 将 `acquireScript` 常量替换为 ZSET 版本**

将 `scripts.go` 中的 `acquireScript` 替换为：

```lua
-- acquire_with_config.lua (ZSET model)
-- KEYS[1]: config_key        (rl:config:<domain>:<api_key>)
-- KEYS[2]: leases_key        (rl:leases:<domain>:<api_key>)
-- KEYS[3]: lease_key         (rl:lease:<lease_id>)
-- KEYS[4]: wildcard_config_key
-- KEYS[5]: global_config_key
-- ARGV[1]: lease_ttl_ms
-- ARGV[2]: now_ms
-- ARGV[3]: lease_id

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

-- 按优先级查找配置：精确 > 通配符 > 全局
local config = get_config(KEYS[1])
if not config then
    config = get_config(KEYS[4])
end
if not config then
    config = get_config(KEYS[5])
end

if not config then
    return cjson.encode({allowed = false, reason = "config_not_found", message = "No rate limit configuration found"})
end

if config.enabled ~= "true" then
    return cjson.encode({allowed = false, reason = "api_key_disabled", message = "API key is disabled"})
end

local max_concurrent = tonumber(config.max_concurrent)
if not max_concurrent or max_concurrent <= 0 then
    return cjson.encode({allowed = false, reason = "invalid_config", message = "Invalid max_concurrent configuration"})
end

-- 清理过期成员
local leases_key = KEYS[2]
local now_ms = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', leases_key, '-inf', now_ms)

-- 检查当前并发数
local current = redis.call('ZCARD', leases_key)
if current >= max_concurrent then
    return cjson.encode({allowed = false, reason = "limit_exceeded", max_concurrent = max_concurrent, current_count = current, message = "Concurrent limit exceeded"})
end

-- 写入 ZSET 和 lease key
local lease_ttl_ms = tonumber(ARGV[1])
local lease_id = ARGV[3]
local expire_at = now_ms + lease_ttl_ms
redis.call('ZADD', leases_key, expire_at, lease_id)
redis.call('SET', KEYS[3], leases_key, 'PX', lease_ttl_ms)

local new_count = current + 1
return cjson.encode({
    allowed = true,
    lease_id = lease_id,
    max_concurrent = max_concurrent,
    current_count = new_count,
    tier = config.tier or "default"
})
```

- [ ] **Step 2: 将 `releaseScript` 常量替换为 ZSET 版本**

将 `scripts.go` 中的 `releaseScript` 替换为：

```lua
-- release_with_lease.lua (ZSET model)
-- KEYS[1]: lease_key  (rl:lease:<lease_id>)
-- ARGV[1]: lease_id   (用于 ZREM)

local leases_key = redis.call('GET', KEYS[1])
if not leases_key then
    return cjson.encode({released = false, reason = "lease_not_found", message = "Lease not found or already expired"})
end

redis.call('DEL', KEYS[1])
redis.call('ZREM', leases_key, ARGV[1])
local current = redis.call('ZCARD', leases_key)

return cjson.encode({released = true, current_count = current})
```

- [ ] **Step 3: 运行脚本相关测试（此时 operations.go 还未改，预期仍失败）**

Run: `go test ./internal/counter-service/redis -count=1`
Expected: FAIL，因为 `operations.go` 还在传旧的 KEYS/ARGV，且 release 仍未传 `ARGV[1]=lease_id`。

---

### Task 3: 更新 operations.go 以匹配新脚本

**Files:**
- Modify: `internal/counter-service/redis/operations.go`

- [ ] **Step 1: 修改 `Acquire` 方法——将 `counterKey` 改为 `leasesKey`，新增 `now_ms`**

将 `operations.go` 中 `Acquire` 方法的 key 构造部分替换为：

```go
func (c *Client) Acquire(ctx context.Context, req models.AcquireRequest) (*AcquireResult, error) {
	leaseID := uuid.New().String()

	configKey  := c.Key(fmt.Sprintf("config:%s:%s", req.Domain, req.APIKey))
	leasesKey  := c.Key(fmt.Sprintf("leases:%s:%s", req.Domain, req.APIKey))
	leaseKey   := c.Key(fmt.Sprintf("lease:%s", leaseID))

	wildcardConfigKey := ""
	if parts := strings.SplitN(req.Domain, ".", 2); len(parts) == 2 {
		wildcardConfigKey = c.Key(fmt.Sprintf("config:*.%s:%s", parts[1], req.APIKey))
	}
	globalConfigKey := c.Key(fmt.Sprintf("config:*:%s", req.APIKey))

	keys := []string{configKey, leasesKey, leaseKey}
	if wildcardConfigKey != "" {
		keys = append(keys, wildcardConfigKey)
	} else {
		keys = append(keys, "") // 占位
	}
	keys = append(keys, globalConfigKey)

	nowMS := time.Now().UnixMilli()
	args := []interface{}{req.TTLMS, nowMS, leaseID}

	result, err := c.rdb.Eval(ctx, GetAcquireScript(), keys, args...).Result()
	// ... 其余解析逻辑不变
```

注意：需要在 import 中添加 `"time"`（如果尚未导入）。

- [ ] **Step 2: 修改 `Release` 方法——新增 `ARGV[1]=lease_id`**

将 `Release` 方法的脚本调用部分替换为：

```go
func (c *Client) Release(ctx context.Context, leaseID string) (*ReleaseResult, error) {
	leaseKey := c.Key(fmt.Sprintf("lease:%s", leaseID))

	result, err := c.rdb.Eval(ctx, GetReleaseScript(), []string{leaseKey}, leaseID).Result()
	// ... 其余解析逻辑不变
```

- [ ] **Step 3: 运行全量 redis 包测试**

Run: `go test ./internal/counter-service/redis -count=1 -v`
Expected: PASS，所有测试通过。

- [ ] **Step 4: 运行 counter-service 全量测试**

Run: `go test ./internal/counter-service/... -count=1`
Expected: PASS

---

### Task 4: 补齐 handler 层对外契约回归测试

**Files:**
- Modify: `internal/counter-service/handler/acquire_test.go`
- Modify: `internal/counter-service/handler/release_test.go`
- Modify: `internal/counter-service/handler/release.go`
- Reference: `internal/counter-service/handler/acquire.go`
- Reference: `internal/counter-service/handler/release.go`

- [ ] **Step 1: 在 handler acquire 测试中补充“lease 过期后再次 acquire 自动恢复”**

先在 `internal/counter-service/handler/acquire_test.go` 的 import 中增加：

```go
import (
    "bytes"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/alicebob/miniredis/v2"
    "rate-limiter-wasm/internal/counter-service/redis"
)
```

然后新增：

```go
func TestAcquireHandler_LeaseExpiredAutoRecovery(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "1")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  100,
	}

	body1, _ := json.Marshal(reqBody)
	req1 := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected first acquire status 200, got %d", w1.Code)
	}

	mr.FastForward(200 * time.Millisecond)

	body2, _ := json.Marshal(reqBody)
	req2 := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected second acquire status 200, got %d", w2.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true after lease expiry, got %#v", resp["allowed"])
	}
}
```

- [ ] **Step 2: 运行 handler acquire 回归测试，确认旧实现失败**

Run: `go test ./internal/counter-service/handler -run TestAcquireHandler_LeaseExpiredAutoRecovery -count=1`
Expected: FAIL，旧模型在 lease TTL 过期后仍会卡住槽位。

- [ ] **Step 3: 在 handler acquire 测试中补充 wildcard/global fallback 回归**

在 `internal/counter-service/handler/acquire_test.go` 中新增两个测试，分别覆盖：

```go
func TestAcquireHandler_WildcardFallback(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:*.example.com:test-key", "max_concurrent", "3")
	mr.HSet("rl:config:*.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true, got %#v", resp["allowed"])
	}
}

func TestAcquireHandler_GlobalFallback(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:*:test-key", "max_concurrent", "4")
	mr.HSet("rl:config:*:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "any.domain.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true, got %#v", resp["allowed"])
	}
}
```

目的：确保 handler 层继续覆盖 wildcard / global fallback 契约，而不仅仅依赖 redis 层测试。

- [ ] **Step 4: 运行 handler fallback 测试**

Run: `go test ./internal/counter-service/handler -run 'TestAcquireHandler_(WildcardFallback|GlobalFallback)' -count=1`
Expected: PASS

- [ ] **Step 5: 在 release handler 中落实 lease_not_found 对外契约，并补全测试断言**

先调整 `internal/counter-service/handler/release.go`：当 `errors.Is(err, redis.ErrLeaseNotFound)` 时，继续返回 `HTTP 200`，但响应体中的 `reason` 必须显式写为 `lease_not_found`，而不是沿用默认的 `internal error`。

再把 `TestReleaseHandler_LeaseNotFound` 的断言补完整为：

```go
if w.Code != http.StatusOK {
    t.Errorf("expected status 200, got %d", w.Code)
}

var resp map[string]interface{}
if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
    t.Fatalf("failed to decode response: %v", err)
}
if released, ok := resp["released"].(bool); !ok || released {
    t.Error("expected released=false")
}
if reason, ok := resp["reason"].(string); !ok || reason != "lease_not_found" {
    t.Errorf("expected reason=lease_not_found, got %#v", resp["reason"])
}
```

说明：redis client 层允许把 lease_not_found 映射为错误返回，但 **handler 层必须继续保持 `200 + released=false + reason=lease_not_found`**。

- [ ] **Step 6: 先运行 release handler 测试确认当前实现失败，再在修改后回归通过**

Run: `go test ./internal/counter-service/handler -run TestReleaseHandler_LeaseNotFound -count=1`
Expected: 先 FAIL（当前 `release.go` 仍返回 `reason=internal error`）；修改 handler 后 PASS，并稳定返回 `200 + released=false + reason=lease_not_found`。

---

## Chunk 2: 运行全量回归并验证无契约漂移

### Task 5: 运行全量测试套件并验证

**Files:**
- Reference: 所有测试文件

- [ ] **Step 1: 运行全量测试**

Run: `go test ./... -count=1`
Expected: PASS，无新增失败。

- [ ] **Step 2: 确认核心回归测试通过**

Run: `go test ./internal/counter-service/redis -run TestAcquire_LeaseExpiredAutoRecovery -count=1 -v`
Expected: PASS，输出显示第二次 acquire 成功。

- [ ] **Step 3: 确认 wildcard/global fallback 测试通过**

Run: `go test ./internal/counter-service/redis -run 'TestAcquire_(WildcardFallback|GlobalFallback)' -count=1 -v`
Expected: PASS

- [ ] **Step 4: 确认 release 幂等测试通过**

Run: `go test ./internal/counter-service/redis -run TestRelease_Idempotent -count=1 -v`
Expected: PASS

- [ ] **Step 5: 尝试构建 WASM artifact（验证无编译错误）**

Run: `bash ./build.sh`
Expected: 成功生成 `dist/rate-limiter.wasm`，无编译错误。

---

## 验收标准

完成后应满足：

1. `go test ./... -count=1` 全部通过
2. `TestAcquire_LeaseExpiredAutoRecovery` 通过（核心回归）
3. `rl:lease:<id>` 的 value 为 `rl:leases:<domain>:<api_key>`（不再是 `rl:counter:...`）
4. `rl:counter:<domain>:<api_key>` 不再被写入
5. `bash ./build.sh` 成功
6. 插件 `/acquire` 与 `/release` 接口契约无变化
