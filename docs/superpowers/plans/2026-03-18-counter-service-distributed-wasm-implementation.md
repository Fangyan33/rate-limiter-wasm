# Counter Service 分布式限流 WASM 落地 Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Counter Service 分布式限流在 WASM 中真实可用：移除 WASM 不安全的 `internal/store` HTTP 实现，统一 `/acquire` 契约为 `HTTP 200 + allowed=true|false` 表达策略结果，并把插件在“已成功 dispatch 后失败”的处理改成 fail-open 透传。

**Architecture:** 插件侧继续作为唯一的 Counter Service 调用入口，通过 `proxywasm.DispatchHttpCall()` 发起 `/acquire` 与 `/release`。Counter Service 负责把 Redis/Lua 的配置查询与并发计数结果翻译成稳定的 HTTP/JSON 契约；`internal/store` 则退回为 WASM-safe 占位实现，仅保留接口与构造约束，不再承载真实网络逻辑。

**Tech Stack:** Go 1.22、TinyGo WASI (`-scheduler=none`)、proxy-wasm-go-sdk、Redis、Lua、Go test、miniredis

---

## File Structure

### Existing files to modify
- `internal/counter-service/handler/acquire.go`
  - 统一 `/acquire` 的 HTTP 状态码契约：策略拒绝返回 `200 + allowed=false`，基础设施错误返回 `503/5xx`。
- `internal/counter-service/handler/acquire_test.go`
  - 覆盖新的 `/acquire` 契约，尤其是“超限/未配置/禁用/非法配置仍返回 200”。
- `internal/plugin/root.go`
  - 删除 async acquire 失败后的本地 fallback 逻辑，改成 fail-open；保留 dispatch 失败时 reject。
- `internal/plugin/root_test.go`
  - 把原本的 fallback 断言改成 fail-open 断言，并补上 release 不应误触发的测试。
- `internal/store/client.go`
  - 移除 `net/http` / `context.WithTimeout` / goroutine，改成 WASM-safe placeholder store。
- `internal/store/client_test.go`
  - 从“真实 HTTP client 构造”改成“占位 store 构造 + Acquire 返回 `limiter.ErrStoreUnavailable`”。
- `docs/superpowers/specs/2026-03-17-counter-service-distributed-wasm-design.md`
  - 如实现中发现与 spec 不一致的小差异，只做最小同步修订。

### Existing files likely to reference while implementing
- `internal/counter-service/models/types.go`
  - 复用 `AcquireRequest` / `AcquireResult` / `ReleaseRequest`。
- `internal/config/config.go`
  - 确认 distributed mode 校验与默认值无需额外改动。
- `build.sh`
  - 用于验证 TinyGo 构建恢复正常。

### No new files required
本次实现应以最小改动为主，不新增 helper/utility 文件，直接在现有边界内完成。

---

## Chunk 1: Counter Service `/acquire` 契约对齐

### Task 1: 先把 `/acquire` 的策略拒绝契约写成失败测试

**Files:**
- Modify: `internal/counter-service/handler/acquire_test.go`
- Reference: `internal/counter-service/handler/acquire.go`
- Reference: `internal/counter-service/models/types.go`

- [ ] **Step 1: 写“超限仍返回 200”失败测试**

```go
func TestAcquireHandler_LimitReachedReturns200WithAllowedFalse(t *testing.T) {
    // 第三次 acquire 命中上限
    // 断言 HTTP 200
    // 断言 body.allowed == false
    // 断言 body.reason == "limit_exceeded"
}
```

- [ ] **Step 2: 运行单测确认当前实现失败**

Run: `go test ./internal/counter-service/handler -run TestAcquireHandler_LimitReachedReturns200WithAllowedFalse -count=1`
Expected: FAIL，当前实现返回 `429`。

- [ ] **Step 3: 再写“配置缺失/禁用/非法配置仍返回 200”测试**

```go
func TestAcquireHandler_ConfigMissReturns200WithAllowedFalse(t *testing.T) {}
func TestAcquireHandler_DisabledConfigReturns200WithAllowedFalse(t *testing.T) {}
func TestAcquireHandler_InvalidConfigReturns200WithAllowedFalse(t *testing.T) {}
```

- [ ] **Step 4: 运行新增测试确认全部失败**

Run: `go test ./internal/counter-service/handler -run 'TestAcquireHandler_(ConfigMiss|DisabledConfig|InvalidConfig|LimitReached).*' -count=1`
Expected: FAIL，当前实现会返回 `404/403/400/429` 之类的状态码。

- [ ] **Step 5: 提交测试骨架**

```bash
git add internal/counter-service/handler/acquire_test.go
git commit -m "test: capture counter service acquire contract"
```

### Task 2: 用最小实现让 `/acquire` 契约通过

**Files:**
- Modify: `internal/counter-service/handler/acquire.go`
- Test: `internal/counter-service/handler/acquire_test.go`

- [ ] **Step 1: 调整错误映射，只让基础设施错误走 5xx**

实现目标：

```go
switch {
case errors.Is(err, redis.ErrRedisUnavailable):
    status = http.StatusServiceUnavailable
    payload = map[string]any{"allowed": false, "reason": "redis_unavailable", "message": err.Error()}
default:
    status = http.StatusOK
    payload = map[string]any{"allowed": false, "reason": mappedReason, "message": err.Error()}
}
```

其中：
- `config_not_found`
- `api_key_disabled`
- `limit_exceeded`
- `invalid_config`

都必须走 `HTTP 200`。

- [ ] **Step 2: 删除“`!result.Allowed` 时改写为 429”的分支**

目标形态：

```go
h.writeJSON(w, http.StatusOK, result)
```

只要 `Acquire` 没返回基础设施错误，就统一返回 200。

- [ ] **Step 3: 运行 handler 包测试**

Run: `go test ./internal/counter-service/handler -count=1`
Expected: PASS

- [ ] **Step 4: 运行 counter-service 相关全量测试，确认没有误伤**

Run: `go test ./internal/counter-service/... -count=1`
Expected: PASS

- [ ] **Step 5: 提交 handler 修复**

```bash
git add internal/counter-service/handler/acquire.go internal/counter-service/handler/acquire_test.go
git commit -m "fix: align acquire responses with wasm contract"
```

---

## Chunk 2: 插件 async acquire 错误处理改为 fail-open

### Task 3: 先把插件当前 fallback 行为改写成失败测试

**Files:**
- Modify: `internal/plugin/root_test.go`
- Reference: `internal/plugin/root.go`

- [ ] **Step 1: 把“非 200 时 fallback 到本地 limiter”的测试改成 fail-open 预期**

将现有测试重命名/改写为：

```go
func TestPluginResumesRequestWhenAsyncAcquireReturnsNon200(t *testing.T) {
    // 第一次请求 pause
    // 回包 500
    // 断言当前流恢复为 ActionContinue
    // 断言没有 local response
}
```

- [ ] **Step 2: 增加“响应 JSON 解析失败时直接放行”测试**

```go
func TestPluginResumesRequestWhenAsyncAcquireResponseIsInvalidJSON(t *testing.T) {}
```

- [ ] **Step 3: 增加“dispatch 自身失败仍 reject”测试**

可通过 host emulator 配置或最小替代路径断言 dispatch error 时仍发送本地拒绝响应。

```go
func TestPluginRejectsWhenAcquireDispatchFails(t *testing.T) {}
```

- [ ] **Step 4: 增加“fail-open 请求不应触发 release callout”测试**

```go
func TestPluginDoesNotDispatchReleaseWhenAcquireFailedOpen(t *testing.T) {}
```

- [ ] **Step 5: 运行插件聚焦测试确认失败**

Run: `go test ./internal/plugin -run 'TestPlugin(ResumesRequestWhenAsyncAcquireReturnsNon200|ResumesRequestWhenAsyncAcquireResponseIsInvalidJSON|RejectsWhenAcquireDispatchFails|DoesNotDispatchReleaseWhenAcquireFailedOpen)' -count=1`
Expected: FAIL，当前实现仍然走 `fallbackToLocalLimiter()`。

- [ ] **Step 6: 提交测试变更**

```bash
git add internal/plugin/root_test.go
git commit -m "test: capture async acquire fail-open behavior"
```

### Task 4: 用最小实现把插件改成 fail-open

**Files:**
- Modify: `internal/plugin/root.go`
- Test: `internal/plugin/root_test.go`

- [ ] **Step 1: 添加一个只负责放行的极小辅助方法**

建议直接在 `internal/plugin/root.go` 内新增：

```go
func (h *httpContext) resumeAfterAcquireFailure(logMessage string, args ...any) {
    proxywasm.LogWarnf(logMessage, args...)
    if err := proxywasm.ResumeHttpRequest(); err != nil {
        proxywasm.LogErrorf("resume http request: %v", err)
    }
}
```

要求：
- 仅用于“已成功 dispatch 后失败”的分支。
- 不引入新文件。

- [ ] **Step 2: 把 `onAcquireResponse` 中的非 200 分支改成直接恢复请求**

目标形态：

```go
if status != "200" {
    h.resumeAfterAcquireFailure("counter service returned status %s, allowing request", status)
    return
}
```

- [ ] **Step 3: 把 body 读取失败 / JSON 解析失败分支改成直接恢复请求**

目标形态：

```go
if err != nil {
    h.resumeAfterAcquireFailure("parse acquire response: %v", err)
    return
}
```

- [ ] **Step 4: 删除不再被使用的 `fallbackToLocalLimiter()`**

要求：
- 如果删掉后有引用编译错误，继续清理所有旧引用。
- 保持 `dispatch` 调用返回 error 时仍走 `h.reject()`。

- [ ] **Step 5: 更新 `newRequestLimiter` 附近注释**

把注释从“local limiter for fallback”调整为更准确的语义，例如：

```go
// Counter service mode uses async HTTP callouts in the plugin layer.
// The local limiter remains only as a non-nil requestLimiter implementation;
// async acquire failures no longer fall back to local limiting.
```

- [ ] **Step 6: 运行插件包测试**

Run: `go test ./internal/plugin -count=1`
Expected: PASS

- [ ] **Step 7: 运行配置包测试，确认 distributed mode 校验仍兼容**

Run: `go test ./internal/config -count=1`
Expected: PASS

- [ ] **Step 8: 提交插件逻辑修复**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go
git commit -m "fix: fail open after async acquire response errors"
```

---

## Chunk 3: `internal/store` 回退为 WASM-safe placeholder

### Task 5: 先把 placeholder 行为写成失败测试

**Files:**
- Modify: `internal/store/client_test.go`
- Reference: `internal/store/client.go`
- Reference: `internal/limiter/distributed.go`

- [ ] **Step 1: 增加“Acquire 返回 `ErrStoreUnavailable`”测试**

```go
func TestPlaceholderClientAcquireReturnsStoreUnavailable(t *testing.T) {
    store, _ := NewClient(validConfig)
    _, allowed, err := store.Acquire("key_basic_001", 1)
    if !errors.Is(err, limiter.ErrStoreUnavailable) { ... }
    if allowed { ... }
}
```

- [ ] **Step 2: 把构造断言改成 placeholder 结构，而不是 `httpCounterServiceClient`**

目标：
- 仍验证 `Name() == "counter_service"`
- 仍验证 `NewClient` 的路径 / TTL / cluster 参数校验
- 不再断言 `http.Client`、timeout、goroutine 相关内部字段

- [ ] **Step 3: 运行 store 包测试确认失败**

Run: `go test ./internal/store -count=1`
Expected: FAIL，当前实现仍会返回真实 HTTP client。

- [ ] **Step 4: 提交测试变更**

```bash
git add internal/store/client_test.go
git commit -m "test: capture wasm-safe placeholder store behavior"
```

### Task 6: 用最小实现移除 WASM 不安全依赖

**Files:**
- Modify: `internal/store/client.go`
- Test: `internal/store/client_test.go`
- Reference: `build.sh`

- [ ] **Step 1: 删除 `net/http`、`context`、`time`、goroutine 相关实现**

保留最小结构，例如：

```go
type client struct{}

func (c *client) Acquire(apiKey string, limit int) (func(), bool, error) {
    return nil, false, limiter.ErrStoreUnavailable
}

func (c *client) Name() string {
    return "counter_service"
}
```

要求：
- 不引入 `net/http`
- 不引入 `context.WithTimeout`
- 不保留 `releaseAsync`
- 不保留 `httpCounterServiceClient`

- [ ] **Step 2: 仅保留 `NewClient` 的配置合法性校验**

目标形态：

```go
func NewClient(cfg config.CounterServiceConfig) (limiter.DistributedStore, error) {
    // 继续校验 cluster / path / ttl
    // 默认 timeout 仍可保留为配置归一化，但不再用于构造 http.Client
    return &client{}, nil
}
```

- [ ] **Step 3: 运行 store 包测试**

Run: `go test ./internal/store -count=1`
Expected: PASS

- [ ] **Step 4: 运行全量 Go 测试**

Run: `go test ./... -count=1`
Expected: PASS

- [ ] **Step 5: 运行 WASM 构建验证 TinyGo 不再触发 goroutine 错误**

Run: `bash ./build.sh`
Expected: 成功输出 `built wasm artifact: .../dist/rate-limiter.wasm`

- [ ] **Step 6: 提交 placeholder 修复**

```bash
git add internal/store/client.go internal/store/client_test.go
git commit -m "fix: replace wasm-unsafe counter service store client"
```

---

## Chunk 4: 最终一致性检查与文档微调

### Task 7: 做最小文档/测试回归确认

**Files:**
- Modify: `docs/superpowers/specs/2026-03-17-counter-service-distributed-wasm-design.md`（仅当实现与文字仍有微小偏差）
- Reference: `deploy/istio/README.md`
- Reference: `deploy/istio/rate-limiter-plugin-config.yaml`

- [ ] **Step 1: 核对实现结果与 spec 是否一致**

重点核对：
- `/acquire` 策略拒绝是否统一返回 `200 + allowed=false`
- 插件是否在 `status!=200` / 解析失败时直接放行
- `DispatchHttpCall` 返回 err 是否仍 reject
- release 是否只在拿到 lease 后触发

- [ ] **Step 2: 仅在必要时修正文档中的表述差异**

如果需要，更新 spec 中对应段落；否则不改文档，避免无意义 diff。

- [ ] **Step 3: 做最终验证**

Run:
- `go test ./... -count=1`
- `bash ./build.sh`

Expected:
- 全部测试通过
- WASM 成功构建

- [ ] **Step 4: 查看工作区确认仅包含本计划相关改动**

Run: `git status --short`
Expected: 只看到本计划涉及文件。

- [ ] **Step 5: 提交最终一致性调整**

```bash
git add docs/superpowers/specs/2026-03-17-counter-service-distributed-wasm-design.md internal/counter-service/handler/acquire.go internal/counter-service/handler/acquire_test.go internal/plugin/root.go internal/plugin/root_test.go internal/store/client.go internal/store/client_test.go
git commit -m "refactor: finalize distributed wasm rate limit flow"
```

---

## Recommended Execution Order

1. 先完成 Chunk 1，锁死 Counter Service `/acquire` 契约。
2. 再完成 Chunk 2，把插件错误处理改成 fail-open。
3. 再完成 Chunk 3，移除 WASM 不安全的 `internal/store` 实现并恢复 TinyGo 构建。
4. 最后执行 Chunk 4 做全量回归与文档一致性确认。

## Guardrails

- 只做本 spec 明确要求的行为变更，不顺手重构其它模块。
- 不新增抽象层；优先修改现有文件。
- 所有行为变更都先写失败测试，再做最小实现。
- 每个 Task 完成后立刻运行对应最小测试命令，不要直接跳到全量测试。
- 任何一步如果出现与 spec 冲突，先更新 spec 再继续实现。
- 最终必须同时满足：
  - `go test ./... -count=1`
  - `bash ./build.sh`

Plan complete and saved to `docs/superpowers/plans/2026-03-18-counter-service-distributed-wasm-implementation.md`. Ready to execute?
