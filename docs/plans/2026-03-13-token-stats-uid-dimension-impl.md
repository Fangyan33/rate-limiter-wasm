# LLM Token 统计（domain, uid）实现计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在现有 rate-limiter Proxy-WASM 插件中新增 LLM Token 用量统计：Prometheus Counter 指标按 `{domain, uid}` 维度输出，并在 `(domain, uid)` 组合数超过上限时归入 `uid="__other__"`。

**Architecture:**
- 在 `OnHttpRequestHeaders` 中：域名命中后解析 `Authorization` 的 JWT payload，提取 `uid`（不验签，依赖上游）；uid 解析失败仅禁用统计，不影响限流/转发。
- 在 `OnHttpRequestBody` 中：解析请求 JSON（如 OpenAI Chat/Completions）提取 `model`（仅用于日志，可选），同时在 `inject_stream_usage` 启用时为流式请求注入 `stream_options.include_usage=true`。
- 在 `OnHttpResponseBody` 中：累积响应体（或流式增量解析），提取 `usage.prompt_tokens` 与 `usage.completion_tokens`。
- 在 `OnHttpStreamDone` 中：按 `(domain, uid)` 更新 `llm_prompt_tokens_total` / `llm_completion_tokens_total` / `llm_stream_parse_errors_total`。
- 指标使用 Envoy stats tag extraction：通过指标名编码 `llm_*;domain=...;uid=...;` 抽取为 Prometheus label。

**Tech Stack:** Go 1.22, Proxy-WASM Go SDK v0.24.0, proxytest host emulator, YAML config via gopkg.in/yaml.v3.

---

## 约束与约定

- **安全边界**：不把 api_key 暴露到 metrics；uid 仅来自 JWT payload（上游已验签）。
- **性能/内存**：为 request/response body 累积设置硬上限（例如 10MB），超限则禁用 token 统计并记录 warn；不影响主流程。
- **高基数控制**：全局 `(domain, uid)` 组合数硬限制默认 5000（可配置）。超过后该 domain 下的新 uid 归入 `__other__`。
- **故障隔离**：JWT 解析失败 / usage 解析失败不影响限流、counter_service 异步 acquire/release 不受影响。

---

## Task 1: 扩展配置模型与默认值（TokenStatistics）

**Files:**
- Modify: [internal/config/config.go](internal/config/config.go)
- Modify: [internal/config/config_test.go](internal/config/config_test.go)

### Step 1: Write the failing test

在 `internal/config/config_test.go` 新增：
- `token_statistics.enabled` 默认 false（不写该段时不影响解析）
- `token_statistics.metric_key_limit` 默认 5000
- `token_statistics.metric_key_limit` 可被配置覆盖

示例测试形状：

```go
func TestParseConfigTokenStatisticsDefaults(t *testing.T) {
    cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
`))
    if err != nil { t.Fatalf("Parse() error = %v", err) }

    if cfg.TokenStatistics.Enabled {
        t.Fatal("expected token_statistics.enabled default false")
    }
    if cfg.TokenStatistics.MetricKeyLimit != 5000 {
        t.Fatalf("expected default metric_key_limit=5000, got %d", cfg.TokenStatistics.MetricKeyLimit)
    }
}
```

```go
func TestParseConfigTokenStatisticsOverridesMetricKeyLimit(t *testing.T) {
    cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
token_statistics:
  enabled: true
  metric_key_limit: 123
`))
    if err != nil { t.Fatalf("Parse() error = %v", err) }

    if !cfg.TokenStatistics.Enabled { t.Fatal("expected enabled") }
    if cfg.TokenStatistics.MetricKeyLimit != 123 {
        t.Fatalf("expected metric_key_limit=123, got %d", cfg.TokenStatistics.MetricKeyLimit)
    }
}
```

### Step 2: Run test to verify it fails

Run: `go test ./internal/config -run TokenStatistics -count=1`

Expected: FAIL（Config 结构体尚无 token_statistics 字段/默认值）。

### Step 3: Write minimal implementation

在 `internal/config/config.go`：
- 增加 `TokenStatistics TokenStatisticsConfig \`yaml:"token_statistics"\``
- 定义 `TokenStatisticsConfig`：
  - `Enabled bool \`yaml:"enabled"\``
  - `InjectStreamUsage bool \`yaml:"inject_stream_usage"\``
  - `MetricKeyLimit int \`yaml:"metric_key_limit"\``
- 在 `applyDefaults()`：
  - `MetricKeyLimit` 若为 0 则设为 5000
- 在 `Validate()`：
  - `MetricKeyLimit` 若 <=0 则报错（当 enabled=true 时必须有效；是否对 enabled=false 也校验由测试决定——建议也校验为正，避免配置陷阱）

### Step 4: Run test to verify it passes

Run: `go test ./internal/config -run TokenStatistics -count=1`

Expected: PASS

---

## Task 2: JWT uid 解析函数（失败禁用统计）

**Files:**
- Modify: [internal/plugin/root.go](internal/plugin/root.go)
- Modify: [internal/plugin/root_test.go](internal/plugin/root_test.go)

### Step 1: Write the failing tests

在 `internal/plugin/root_test.go` 新增单元测试（通过 host emulator 驱动 request headers）：
- 有效 JWT（payload: `{ "uid": "123" }`）→ 统计启用、uid=123（通过后续 metrics 验证）
- JWT 不是三段 / payload base64url decode 失败 / claims 不含 uid → 不影响请求继续（限流仍生效），但不产生 metrics

> 说明：由于 plugin 当前没有 metrics，我们会在 Task 3 引入 metrics 后，用 metrics 结果来间接验证 uid 解析是否生效。

### Step 2: Run test to verify it fails

Run: `go test ./internal/plugin -run JWT -count=1`

Expected: FAIL（尚无 uid 解析逻辑/测试依赖的行为不存在）。

### Step 3: Minimal implementation

在 `internal/plugin/root.go`：
- 扩展 `httpContext`：
  - `tokenStatsEnabled bool`
  - `domain string`
  - `uid string`
- `OnHttpRequestHeaders`：
  - domain 命中时记录 `h.domain`
  - 如果 `cfg.TokenStatistics.Enabled`：解析 uid；失败则 `tokenStatsEnabled=false` 并 `LogWarnf`；成功则记录 uid 并 `tokenStatsEnabled=true`
- 新增 `parseUIDFromJWT(authHeader string) (string, error)`（按设计文档：长度限制 16KB、uid 长度限制 64、支持 string/number）

### Step 4: Re-run tests

Run: `go test ./internal/plugin -run JWT -count=1`

Expected: PASS

---

## Task 3: 指标缓存 + 基数限制（domain|uid）

**Files:**
- Modify: [internal/plugin/root.go](internal/plugin/root.go)
- Modify: [internal/plugin/root_test.go](internal/plugin/root_test.go)

### Step 1: Write failing tests for metrics

新增测试验证：
1) 当响应 usage=prompt=10, completion=20 且 uid 解析成功时，host 侧能读到 counters：
- `llm_prompt_tokens_total;domain=api.example.com;uid=123;` 累加 10
- `llm_completion_tokens_total;domain=api.example.com;uid=123;` 累加 20

2) 当 `(domain, uid)` 组合超过 `metric_key_limit`（测试可设为 2），第 3 个 uid 归入 `__other__`。

测试需要：
- 在 proxytest HostEmulator 用 `GetCounterMetric(name)` 读取指标值
- 为了触发更新，测试会驱动：
  - RequestHeaders（含 Authorization）
  - ResponseBody（包含 usage JSON）
  - StreamDone

### Step 2: Run test to verify it fails

Run: `go test ./internal/plugin -run TokenStats -count=1`

Expected: FAIL（尚未定义/增量 metric）。

### Step 3: Minimal implementation

在 `rootContext` 增加：
- `metricPromptTokens map[string]proxywasm.MetricCounter`
- `metricCompletionTokens map[string]proxywasm.MetricCounter`
- `metricParseErrors map[string]proxywasm.MetricCounter`
- `metricKeyCount int`
- `metricKeyLimit int`（从 cfg.TokenStatistics.MetricKeyLimit 读取，默认 5000）

实现：
- `sanitizeMetricValue(string) string`：替换 `;` `=` 等为 `_`（domain 也要 sanitize）
- `getPromptCounter(domain, uid string)` / `getCompletionCounter(...)` / `getParseErrorsCounter(...)`：
  - key = `domain|uid`
  - 命中缓存直接返回
  - 若 `metricKeyCount >= metricKeyLimit`：使用 `uid=__other__`（不增加 metricKeyCount）
  - 创建 counter：`proxywasm.DefineCounterMetric(fmt.Sprintf("llm_prompt_tokens_total;domain=%s;uid=%s;", domain, uid))`

在 `LoadConfiguration` 初始化 map，并写入 `metricKeyLimit`。

---

## Task 4: usage 解析 + 流式注入（最小实现）

**Files:**
- Modify: [internal/plugin/root.go](internal/plugin/root.go)
- Modify: [internal/plugin/root_test.go](internal/plugin/root_test.go)

### Step 1: Write failing tests

新增测试：
- 请求为 stream=true 且 `inject_stream_usage=true` 时，在 `OnHttpRequestBody` 结束时 request body 被改写，包含 `"stream_options":{"include_usage":true}`。
- 响应 body（非流式）包含：`{"usage":{"prompt_tokens":1,"completion_tokens":2}}` → metrics 累加。
- usage JSON 缺失或解析失败 → `llm_stream_parse_errors_total` 累加 1。

### Step 2: Verify RED

Run: `go test ./internal/plugin -run Usage -count=1`

Expected: FAIL

### Step 3: Minimal implementation

- `OnHttpRequestBody`：读取完整 request body（只在 endOfStream=true 时解析），如果 inject 开启则解析 JSON 并在需要时写回（用 `proxywasm.ReplaceHttpRequestBody`）。
- `OnHttpResponseHeaders`：记录 content-type（可选）
- `OnHttpResponseBody`：在 endOfStream=true 时读取完整 response body 并解析 usage。

> 注意：这一步的“流式增量解析”可能需要更大工作量；先做“非流式或全量累积”以满足测试。后续再按需要扩展。

---

## Task 5: StreamDone 更新指标 + 日志脱敏

**Files:**
- Modify: [internal/plugin/root.go](internal/plugin/root.go)

### Step 1: Write failing test

驱动一次完整请求/响应后，确认 counters 在 StreamDone 之后已经累加（否则 body 阶段也可累加——但以 StreamDone 为准，符合文档）。

### Step 2: Implement minimal

在 `OnHttpStreamDone`：
- 保持现有限流 release + counter_service release 不变
- 若 `tokenStatsEnabled` 且 tokens>0：调用 `updateTokenMetrics()`

---

## Task 6: 更新部署示例 YAML

**Files:**
- Modify: [deploy/istio/rate-limiter-plugin-config.yaml](deploy/istio/rate-limiter-plugin-config.yaml)
- Modify: [deploy/istio/rate-limiter-envoyfilter.yaml](deploy/istio/rate-limiter-envoyfilter.yaml)

### Step 1: Update plugin config example

追加：

```yaml
token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
```

### Step 2: Update EnvoyFilter example

在 `spec.configPatches` 添加一个 `applyTo: BOOTSTRAP` patch，示例：

```yaml
- applyTo: BOOTSTRAP
  patch:
    operation: MERGE
    value:
      stats_config:
        stats_tags:
        - tag_name: domain
          regex: ";domain=([^;]+);"
        - tag_name: uid
          regex: ";uid=([^;]+);"
```

---

## Task 7: Full verification

Run:
- `go test ./... -count=1`

Expected:
- 全绿
- 新增测试覆盖 token_statistics config、JWT uid 解析、usage 解析、metric_key_limit 超限归并。
