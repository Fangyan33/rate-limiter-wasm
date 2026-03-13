# LLM Token 用量统计功能 - uid 维度设计方案

## 1. 设计目标

在现有 Token 统计功能基础上，将 Prometheus 指标维度从 `{domain, api_key, model}` 调整为 `{domain, uid}`：

- **uid 来源**：从 JWT claim `uid` 字段解析（上游已验签，WASM 只解析）
- **基数控制**：`(domain, uid)` 组合数硬限制 ≤ 5000，超出部分归入 `uid="__other__"`
- **安全边界**：不暴露 api_key 到 metrics；uid 解析失败不影响主流程（限流/转发）
- **性能要求**：P99 延迟增加 < 1ms

---

## 2. 指标定义

### 2.1 目标 Prometheus 形态

```
llm_prompt_tokens_total{domain="<domain>", uid="<uid>"} - Counter
llm_completion_tokens_total{domain="<domain>", uid="<uid>"} - Counter
llm_stream_parse_errors_total{domain="<domain>", uid="<uid>"} - Counter
```

### 2.2 维度说明

- **domain**：归一化后的 `:authority`（仅命中 `domains` 配置的请求）
- **uid**：从 JWT payload 的 `uid` claim 解析
- **不包含**：api_key（避免凭证泄露）、model（避免高基数）

### 2.3 基数估算

- 假设 domain = 20，uid = 5000，指标数 = 3
- 总 time series ≈ 20 × 5001 × 3 = 300,060（包含 `__other__`）
- Prometheus 通常可接受（需根据实际环境验证）

---

## 3. 技术实现方案

### 3.1 整体架构

```
请求流程：
1. OnHttpRequestHeaders:
   - 域名匹配 + 记录 domain
   - 解析 JWT，提取 uid（失败则禁用统计）
   - API Key 校验 + 并发限流（不变）

2. OnHttpRequestBody:
   - 读取请求体，解析 model
   - 检测流式请求并注入 stream_options（不变）

3. OnHttpResponseHeaders:
   - 检查响应 Content-Type（不变）

4. OnHttpResponseBody:
   - 累积响应体，解析 Token usage（不变）

5. OnHttpStreamDone:
   - 释放并发计数（不变）
   - 更新 Prometheus 指标（修改：使用 domain + uid 维度）
```

### 3.2 JWT 解析策略（无验签）

#### 前提条件
- 上游 Istio/Envoy `RequestAuthentication` 已验签
- WASM 只负责解析 payload，信任上游验证结果

#### 解析流程
1. **获取 token**：从 `Authorization: Bearer <token>` 提取（复用现有 `auth.ParseBearerToken`）
2. **分割 JWT**：按 `.` 分成 `[header, payload, signature]`，只取 `payload`
3. **base64url decode**：
   ```go
   // Go 标准库，支持无 padding
   decoded, err := base64.RawURLEncoding.DecodeString(payload)
   ```
4. **JSON 解析**：
   ```go
   var claims map[string]interface{}
   json.Unmarshal(decoded, &claims)
   ```
5. **提取 uid**：
   ```go
   uid, ok := claims["uid"]
   if !ok {
       // uid 缺失，禁用统计
   }
   // 类型处理：string 直接用，number 转 string
   ```

#### 错误处理（失败直接禁用统计）
- JWT 格式不对（不是三段）→ `h.tokenStatsEnabled = false`
- payload decode 失败 → `h.tokenStatsEnabled = false`
- JSON 解析失败 → `h.tokenStatsEnabled = false`
- `uid` 字段缺失/null/类型异常 → `h.tokenStatsEnabled = false`
- **不记录到 `llm_stream_parse_errors_total`**（这是 JWT 解析错误，不是 usage 解析错误）
- 记日志：`proxywasm.LogWarnf("failed to parse uid from JWT: %v", err)`

#### 安全边界
- `Authorization` header 长度上限：16KB（超过直接禁用统计）
- payload decode 后 JSON 大小上限：64KB（可选）
- uid 长度上限：64 字符（超长禁用统计或截断）
- uid 值 sanitize：移除 `;` 等特殊字符（避免破坏指标名编码）

---

## 4. 指标实现细节

### 4.1 rootContext 结构扩展

```go
type rootContext struct {
    // ... 现有字段

    // Token 统计指标缓存（修改）
    // key: "domain|uid" (例如 "api.openai.com|12345")
    metricPromptTokens     map[string]proxywasm.MetricCounter
    metricCompletionTokens map[string]proxywasm.MetricCounter
    metricParseErrors      map[string]proxywasm.MetricCounter

    // uid 硬限制（新增）
    metricKeyCount int  // 已创建的 (domain, uid) 组合数
    metricKeyLimit int  // 硬限制上限，默认 5000
}
```

### 4.2 懒加载 counter 逻辑（带硬限制）

```go
func (r *rootContext) getPromptTokensCounter(domain, uid string) proxywasm.MetricCounter {
    key := domain + "|" + uid

    // 检查缓存
    if counter, exists := r.metricPromptTokens[key]; exists {
        return counter
    }

    // 检查硬限制
    if r.metricKeyCount >= r.metricKeyLimit {
        // 超过限制，使用 __other__
        otherKey := domain + "|__other__"
        if counter, exists := r.metricPromptTokens[otherKey]; exists {
            return counter
        }
        // 为 __other__ 创建 counter（不计入 metricKeyCount）
        counter := proxywasm.DefineCounterMetric(
            fmt.Sprintf("llm_prompt_tokens_total;domain=%s;uid=__other__;", domain),
        )
        r.metricPromptTokens[otherKey] = counter
        return counter
    }

    // 创建新 counter
    r.metricKeyCount++
    counter := proxywasm.DefineCounterMetric(
        fmt.Sprintf("llm_prompt_tokens_total;domain=%s;uid=%s;", domain, sanitizeUID(uid)),
    )
    r.metricPromptTokens[key] = counter
    return counter
}

// sanitizeUID 移除特殊字符，避免破坏指标名编码
func sanitizeUID(uid string) string {
    // 移除 ; = 等字符，或替换为 _
    return strings.ReplaceAll(strings.ReplaceAll(uid, ";", "_"), "=", "_")
}
```

### 4.3 指标名编码规范

插件侧按以下格式编码指标名（供 Envoy stats tag extraction 使用）：

```
llm_prompt_tokens_total;domain=<domain>;uid=<uid>;
llm_completion_tokens_total;domain=<domain>;uid=<uid>;
llm_stream_parse_errors_total;domain=<domain>;uid=<uid>;
```

注意：
- 分号 `;` 作为分隔符
- `key=value` 格式
- 末尾也有分号（Envoy stats tag 约定）
- domain 和 uid 值需要 sanitize

---

## 5. Envoy/Istio 配置

### 5.1 Stats Tag Extraction 配置

Stats tag extraction 属于 Envoy 的 **BOOTSTRAP** 级配置。为减少部署对象数量，建议**合并到现有的** `deploy/istio/rate-limiter-envoyfilter.yaml` 中：在该 EnvoyFilter 的 `spec.configPatches` 下新增一个 `applyTo: BOOTSTRAP` 的 patch，将 `domain` 与 `uid` 从指标名中抽取为 Prometheus label。

示例（需要把这一段合并进 `rate-limiter-envoyfilter.yaml` 的 `configPatches` 列表中，而不是创建新的 EnvoyFilter 资源）：

```yaml
# deploy/istio/rate-limiter-envoyfilter.yaml
spec:
  configPatches:
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

### 5.2 插件配置示例

在 `deploy/istio/rate-limiter-plugin-config.yaml` 中添加 Token 统计配置：

```yaml
token_statistics:
  enabled: true                   # 是否启用 Token 统计
  inject_stream_usage: true       # 是否自动为流式请求注入 include_usage 参数
  metric_key_limit: 5000          # (domain, uid) 组合数硬限制
```

---

## 6. httpContext 状态扩展

### 6.1 结构体修改

```go
type httpContext struct {
    types.DefaultHttpContext
    root                *rootContext
    release             func()
    pendingAcquire      bool
    distributedAPIKey   string
    distributedLeaseID  string

    // Token 统计相关状态（修改）
    tokenStatsEnabled   bool     // 是否启用 Token 统计
    domain              string   // 归一化后的域名
    uid                 string   // 从 JWT 解析的 uid（新增）
    requestBody         []byte   // 累积的请求体
    responseContentType string   // 响应 Content-Type
    responseBody        []byte   // 累积的响应体
    model               string   // 模型名称
    promptTokens        int      // 输入 Token 数
    completionTokens    int      // 输出 Token 数
}
```

### 6.2 OnHttpRequestHeaders 修改

```go
func (h *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
    // 1. 域名匹配（现有逻辑）
    host, err := proxywasm.GetHttpRequestHeader(":authority")
    if err != nil {
        return types.ActionContinue
    }

    if !h.root.matcher.Match(normalizeHost(host)) {
        return types.ActionContinue
    }

    // 记录 domain
    h.domain = normalizeHost(host)

    // 2. 读取 Authorization header（现有逻辑）
    authorization, err := proxywasm.GetHttpRequestHeader("authorization")
    if err != nil {
        return h.reject()
    }

    // 3. 解析 Bearer token（现有逻辑，用于限流）
    apiKey, err := auth.ParseBearerToken(authorization)
    if err != nil {
        return h.reject()
    }

    // 4. 新增：解析 JWT，提取 uid（用于统计）
    if h.root.cfg.TokenStatistics.Enabled {
        uid, err := parseUIDFromJWT(authorization)
        if err != nil {
            // JWT 解析失败，禁用统计，但不影响主流程
            proxywasm.LogWarnf("failed to parse uid from JWT: %v", err)
            h.tokenStatsEnabled = false
        } else {
            h.tokenStatsEnabled = true
            h.uid = uid
        }
    }

    // 5. 并发限流逻辑（现有逻辑，不变）
    // ...

    return types.ActionContinue
}

// parseUIDFromJWT 从 JWT payload 提取 uid claim
func parseUIDFromJWT(authHeader string) (string, error) {
    // 1. 提取 Bearer token
    token := strings.TrimPrefix(authHeader, "Bearer ")
    token = strings.TrimSpace(token)

    // 2. 长度检查
    if len(token) > 16*1024 {
        return "", fmt.Errorf("token too large")
    }

    // 3. 分割 JWT
    parts := strings.Split(token, ".")
    if len(parts) != 3 {
        return "", fmt.Errorf("invalid JWT format")
    }

    // 4. base64url decode payload
    payload, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        return "", fmt.Errorf("decode payload: %w", err)
    }

    // 5. JSON 解析
    var claims map[string]interface{}
    if err := json.Unmarshal(payload, &claims); err != nil {
        return "", fmt.Errorf("unmarshal claims: %w", err)
    }

    // 6. 提取 uid
    uidValue, ok := claims["uid"]
    if !ok {
        return "", fmt.Errorf("uid claim not found")
    }

    // 7. 类型处理
    var uid string
    switch v := uidValue.(type) {
    case string:
        uid = v
    case float64:
        uid = fmt.Sprintf("%.0f", v)
    default:
        return "", fmt.Errorf("uid type invalid: %T", v)
    }

    // 8. 长度检查
    if len(uid) > 64 {
        return "", fmt.Errorf("uid too long")
    }

    return uid, nil
}
```

### 6.3 OnHttpStreamDone 修改

```go
func (h *httpContext) OnHttpStreamDone() {
    // 1. 释放并发计数（现有逻辑，不变）
    if h.release != nil {
        h.release()
    }

    // 2. 分布式限流释放（现有逻辑，不变）
    // ...

    // 3. 更新 Token 统计指标（修改：使用 domain + uid）
    if h.tokenStatsEnabled && (h.promptTokens > 0 || h.completionTokens > 0) {
        h.updateTokenMetrics()
    }
}

func (h *httpContext) updateTokenMetrics() {
    if h.root == nil {
        return
    }

    // 更新输入 Token 指标
    if h.promptTokens > 0 {
        counter := h.root.getPromptTokensCounter(h.domain, h.uid)
        if counter != 0 {
            proxywasm.IncrementMetric(counter, uint64(h.promptTokens))
        }
    }

    // 更新输出 Token 指标
    if h.completionTokens > 0 {
        counter := h.root.getCompletionTokensCounter(h.domain, h.uid)
        if counter != 0 {
            proxywasm.IncrementMetric(counter, uint64(h.completionTokens))
        }
    }

    // 记录日志（用于调试）
    proxywasm.LogInfof(
        "token_usage domain=%s uid=%s model=%s prompt_tokens=%d completion_tokens=%d",
        h.domain,
        maskUID(h.uid),
        h.model,
        h.promptTokens,
        h.completionTokens,
    )
}

// maskUID 脱敏 uid（日志用）
func maskUID(uid string) string {
    if len(uid) <= 4 {
        return "****"
    }
    return uid[:2] + "****" + uid[len(uid)-2:]
}
```

---

## 7. 需要改动的文件清单

### 核心实现文件（2 个）：
1. **`internal/config/config.go`** - 配置模型扩展
   - 新增 `TokenStatisticsConfig.MetricKeyLimit` 字段（默认 5000）

2. **`internal/plugin/root.go`** - 核心逻辑实现
   - rootContext 添加 `metricKeyCount`、`metricKeyLimit` 字段
   - 修改指标缓存 key 为 `domain|uid`
   - 新增 `parseUIDFromJWT()` 函数
   - 修改 `OnHttpRequestHeaders()` 添加 JWT 解析逻辑
   - 修改 `getPromptTokensCounter()` 等方法支持硬限制
   - 修改 `updateTokenMetrics()` 使用 `(domain, uid)` 维度

### 配置示例文件（2 个）：
3. **`deploy/istio/rate-limiter-plugin-config.yaml`** - 插件配置示例
   - 添加 `token_statistics.metric_key_limit` 配置

4. **`deploy/istio/rate-limiter-envoyfilter.yaml`** - EnvoyFilter 配置示例（修改）
   - 在 `configPatches` 中新增 `applyTo: BOOTSTRAP` 的 patch
   - 配置 `stats_config.stats_tags` 提取 `domain` 和 `uid`

### 测试文件（2 个）：
5. **`internal/config/config_test.go`** - 配置解析测试
   - 测试 `metric_key_limit` 配置解析

6. **`internal/plugin/root_test.go`** - 插件功能测试
   - 测试 JWT 解析成功/失败场景
   - 测试 uid 硬限制（超过 5000 归入 `__other__`）
   - 测试指标更新使用 `(domain, uid)` 维度

**总计：6 个文件**

---

## 8. 测试验证方案

### 8.1 单元测试

#### `internal/config/config_test.go`
- Token 统计配置解析测试
- `metric_key_limit` 默认值测试（5000）
- 配置禁用时的行为测试

#### `internal/plugin/root_test.go`
- **JWT 解析测试**：
  - 正常 JWT 提取 uid 成功
  - JWT 格式错误（不是三段）→ 禁用统计
  - payload decode 失败 → 禁用统计
  - uid claim 缺失 → 禁用统计
  - uid 类型为 number → 转 string 成功
  - Authorization header 超长 → 禁用统计
- **uid 硬限制测试**：
  - 前 5000 个 `(domain, uid)` 正常创建 counter
  - 第 5001 个归入 `uid="__other__"`
  - `__other__` counter 正常累加
- **指标更新测试**：
  - 验证指标名格式：`llm_prompt_tokens_total;domain=...;uid=...;`
  - 验证 `(domain, uid)` 维度正确
- **容错测试**：
  - JWT 解析失败不影响限流逻辑
  - JWT 解析失败不影响请求转发

### 8.2 集成测试

1. 部署插件到 Istio 环境
2. 配置 `EnvoyFilter` 启用 stats tag extraction
3. 发送带有效 JWT 的请求，验证：
   - uid 正确提取
   - Token 统计正常
   - 访问 Envoy `/stats/prometheus`，验证指标格式：
     ```
     llm_prompt_tokens_total{domain="api.openai.com",uid="12345"} 100
     ```
4. 发送无效 JWT 的请求，验证：
   - 请求仍然正常转发（不影响主流程）
   - Token 统计被禁用
5. 发送超过 5000 个不同 uid 的请求，验证：
   - 前 5000 个正常统计
   - 后续归入 `uid="__other__"`
6. 发送域名未命中的请求，验证不触发统计

### 8.3 性能验证

1. 验证 JWT 解析开销 < 0.5ms（P99）
2. 验证请求体和响应体累积不会导致内存溢出（需要设置最大 body size 限制）
3. 验证 P99 延迟增加 < 1ms（符合 PRD 性能要求）
4. 验证 `metricKeyCount` 达到 5000 后不再增长（内存稳定）

---

## 9. 风险与注意事项

### 9.1 JWT 解析失败率
**问题**：如果 JWT 格式不规范或 uid claim 缺失，会导致大量请求无法统计

**缓解措施**：
- 记录详细日志，方便排查 JWT 格式问题
- 提供配置开关，允许降级到"不解析 uid"模式（例如使用 api_key hash）
- 监控 JWT 解析失败率（可通过日志或新增专用指标）

### 9.2 uid 硬限制的运营影响
**问题**：超过 5000 的 uid 归入 `__other__`，会丢失细粒度统计

**缓解措施**：
- 监控 `__other__` 的流量占比，如果过高说明需要调整限制
- 提供配置参数 `metric_key_limit`，允许根据实际情况调整
- 考虑 LRU 策略（可选）：淘汰最久未使用的 uid，为新 uid 腾出空间

### 9.3 内存占用风险
**问题**：累积完整请求体和响应体可能导致内存溢出

**缓解措施**（现有设计已包含）：
- 设置最大 body size 限制（如 10MB）
- 超过限制时跳过 Token 统计，记录警告日志

### 9.4 安全边界
**问题**：虽然不暴露 api_key，但 uid 仍可能是敏感标识

**缓解措施**：
- 日志中对 uid 做脱敏（`maskUID()`）
- 确保 Prometheus metrics 端点访问受控（不对外暴露）
- 考虑对 uid 做哈希（可选，但会影响可读性）

### 9.5 Envoy stats tag extraction 配置复杂度
**问题**：stats tag extraction 属于 Envoy bootstrap 级别配置，可能影响全局

**缓解措施**：
- 在测试环境充分验证 `EnvoyFilter` 配置
- 确认 regex 不会误匹配其他指标
- 文档化配置步骤，方便运维团队部署

---

## 10. 后续扩展方向

1. **LRU 策略**：当 uid 数接近上限时，淘汰最久未使用的 uid
2. **uid 白名单**：仅为重点用户/付费用户输出细粒度指标
3. **model 维度**：在 uid 数可控的前提下，考虑增加 model 维度（需评估基数）
4. **JWT 验签**：如果上游未验签，WASM 内实现 JWT 验签（需要 JWK 拉取/缓存）
5. **多 claim 支持**：支持从多个 claim 字段提取 uid（例如 `uid` → `user_id` → `sub` 的 fallback 逻辑）
6. **实时监控面板**：基于 `{domain, uid}` 维度构建 Grafana 看板

---

## 11. 与现有设计的差异

与 [token-statistics-implementation.md](token-statistics-implementation.md) 的主要差异：

| 维度 | 现有设计 | 本设计 |
|------|---------|--------|
| 指标维度 | `{domain}` | `{domain, uid}` |
| uid 来源 | 无 | JWT claim `uid` |
| api_key 暴露 | 不暴露 | 不暴露 |
| model 暴露 | 不暴露 | 不暴露 |
| 基数控制 | per-domain map | `(domain, uid)` 组合数 ≤ 5000 |
| 超限策略 | 无 | 归入 `uid="__other__"` |
| JWT 解析 | 无 | 新增，失败禁用统计 |
| 安全验证 | 无 | 依赖上游 Istio JWT 验签 |

---

## 12. 参考资料

- [Proxy-WASM Go SDK - Metrics Example](https://github.com/tetratelabs/proxy-wasm-go-sdk)
- [Envoy Proxy - Statistics](https://www.envoyproxy.io/docs/envoy/latest/operations/stats_overview)
- [Envoy Proxy - Stats Tags](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/metrics/v3/stats.proto#config-metrics-v3-statsconfig)
- [Istio - EnvoyFilter](https://istio.io/latest/docs/reference/config/networking/envoy-filter/)
- [JWT RFC 7519](https://datatracker.ietf.org/doc/html/rfc7519)
- [OpenAI API - Streaming](https://platform.openai.com/docs/api-reference/streaming)

---

## 13. 总结

本设计方案在现有 Token 统计功能基础上，通过以下关键设计实现 `{domain, uid}` 维度的 Prometheus 指标：

1. **JWT 解析**：从 `Authorization` header 提取 JWT，解析 payload 的 `uid` claim（无验签，依赖上游）
2. **硬限制保护**：`(domain, uid)` 组合数 ≤ 5000，超出归入 `uid="__other__"`，确保内存和 time series 可控
3. **失败隔离**：JWT 解析失败仅禁用统计，不影响限流和请求转发
4. **指标编码**：使用 `llm_*;domain=...;uid=...;` 格式，配合 Envoy stats tag extraction 实现 Prometheus label
5. **安全边界**：不暴露 api_key，uid 在日志中脱敏，依赖上游 JWT 验签保证身份可信

核心改动集中在 `internal/plugin/root.go`（JWT 解析 + 指标更新）和 `internal/config/config.go`（配置扩展），不影响现有的限流逻辑。实现分为 6 个文件改动，每个步骤都有明确的代码示例和测试用例，确保功能的正确性和稳定性。
