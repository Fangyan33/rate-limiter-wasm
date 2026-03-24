# rate-limiter-wasm e2e 用例分解清单（请求报文 + 断言字段）

> 日期：2026-03-24
> 依赖文档：`docs/plans/2026-03-24-e2e-test-requirements.md`
> 目的：作为后续 e2e 自动化编码实现蓝图

## 1. 约定与命名

1. 用例 ID：`E2E-<Layer>-<NNN>`
2. Layer 划分：
   - `CS`: counter-service API 层
   - `GW`: ingress + wasm 数据面层
   - `MET`: 指标与可观测性层
3. 断言字段记法：
   - HTTP 状态：`status`
   - Header：`headers["content-type"]`
   - JSON 字段：`$.allowed` / `$.reason`（JSONPath）

## 2. 全局前置条件

1. 已部署 redis、counter-service、nginx upstream、envoyfilter、gateway/virtualservice。
2. 初始化配置（通过 `/config`）：
   - `domain=llm-demo.local, api_key=key_basic_001, max_concurrent=1, enabled=true`
   - `domain=llm-demo.local, api_key=key_premium_001, max_concurrent=2, enabled=true`
   - `domain=*.example.com, api_key=key_basic_001, max_concurrent=1, enabled=true`
   - `domain=*, api_key=key_global_001, max_concurrent=1, enabled=true`
   - `domain=llm-demo.local, api_key=key_disabled_001, max_concurrent=1, enabled=false`
3. gateway 地址记为 `${GATEWAY}`，counter-service 地址记为 `${COUNTER}`。
4. 默认请求路径：`/mock/v1/chat/completions`（返回包含 usage 的 JSON）。

---

## 3. CS 层用例（Counter Service API）

### E2E-CS-001 健康检查

**Request**
```http
GET /health HTTP/1.1
Host: counter-service:8080
```

**断言字段**
1. `status == 200`
2. `body == "OK"`

---

### E2E-CS-002 创建配置（PUT /config）

**Request**
```http
PUT /config HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"api.example.com","api_key":"key_put_001","max_concurrent":2,"enabled":true,"tier":"basic"}
```

**断言字段**
1. `status == 200`
2. `$.status == "ok"`

---

### E2E-CS-003 查询配置（GET /config）

**Request**
```http
GET /config?domain=api.example.com&api_key=key_put_001 HTTP/1.1
Host: counter-service:8080
```

**断言字段**
1. `status == 200`
2. `$.domain == "api.example.com"`
3. `$.api_key == "key_put_001"`
4. `$.max_concurrent == 2`
5. `$.enabled == true`

---

### E2E-CS-004 列表查询（GET /configs）

**Request**
```http
GET /configs?cursor=0&limit=100 HTTP/1.1
Host: counter-service:8080
```

**断言字段**
1. `status == 200`
2. `$.cursor` 存在
3. `$.configs` 为数组且长度 > 0

---

### E2E-CS-005 删除配置（DELETE /config）

**Request**
```http
DELETE /config?domain=api.example.com&api_key=key_put_001 HTTP/1.1
Host: counter-service:8080
```

**断言字段**
1. `status == 200`
2. `$.status == "deleted"`

**后置验证请求**
```http
GET /config?domain=api.example.com&api_key=key_put_001 HTTP/1.1
Host: counter-service:8080
```

**后置断言**
1. `status == 404`
2. `$.error == "config not found"`

---

### E2E-CS-010 Acquire 成功

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"llm-demo.local","api_key":"key_basic_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == true`
3. `$.lease_id` 非空
4. `$.max_concurrent == 1`
5. `$.current_count == 1`

---

### E2E-CS-011 Acquire 超限拒绝（HTTP 200）

**步骤**
1. 先执行一次 E2E-CS-010 占满
2. 再发同 key acquire

**Request（第 2 次）**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"llm-demo.local","api_key":"key_basic_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == false`
3. `$.reason == "limit_exceeded"`
4. `$.max_concurrent == 1`
5. `$.current_count == 1`

---

### E2E-CS-012 Acquire 配置缺失拒绝（HTTP 200）

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"llm-demo.local","api_key":"key_missing_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == false`
3. `$.reason == "config_not_found"`

---

### E2E-CS-013 Acquire disabled key 拒绝（HTTP 200）

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"llm-demo.local","api_key":"key_disabled_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == false`
3. `$.reason == "api_key_disabled"`

---

### E2E-CS-014 Release 成功

**前置**：先 acquire 一次拿到 `lease_id`。

**Request**
```http
POST /release HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"lease_id":"<LEASE_ID>"}
```

**断言字段**
1. `status == 200`
2. `$.released == true`
3. `$.current_count == 0`

---

### E2E-CS-015 Release 幂等

**步骤**
1. 使用同一个 `lease_id` 连续调用 `/release` 两次

**第 2 次断言字段**
1. `status == 200`
2. `$.released == false`
3. `$.reason == "lease_not_found"`

---

### E2E-CS-016 TTL 过期自动恢复

**步骤**
1. `ttl_ms=100` acquire 成功
2. 等待 >100ms
3. 再次 acquire

**第 2 次断言字段**
1. `status == 200`
2. `$.allowed == true`
3. `$.current_count == 1`

---

### E2E-CS-017 wildcard 回退

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"foo.example.com","api_key":"key_basic_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == true`
3. `$.max_concurrent == 1`

---

### E2E-CS-018 global 回退

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"any.unknown.tld","api_key":"key_global_001","ttl_ms":30000}
```

**断言字段**
1. `status == 200`
2. `$.allowed == true`
3. `$.max_concurrent == 1`

---

### E2E-CS-019 Redis 不可用

**步骤**
1. 暂停/断开 redis
2. 发起 acquire

**Request**
```http
POST /acquire HTTP/1.1
Host: counter-service:8080
Content-Type: application/json

{"domain":"llm-demo.local","api_key":"key_basic_001","ttl_ms":30000}
```

**断言字段**
1. `status == 503`
2. `$.allowed == false`
3. `$.reason == "redis_unavailable"`

---

## 4. GW 层用例（Ingress + WASM + Counter Service）

### E2E-GW-001 域名未命中 bypass

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: unmatched.local
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. 请求不被 wasm 拒绝（`status != 429`）
2. 请求可到达 upstream（可通过响应体特征字段断言）

---

### E2E-GW-002 域名命中但缺失 Authorization

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. `status == 429`
2. `headers["content-type"]` 包含 `text/plain`
3. `body == "Rate limit exceeded"`（或等于当前配置的 `error_response.message`）

---

### E2E-GW-003 Authorization 非 Bearer

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Basic abc
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. `status == 429`
2. `headers["content-type"]` 包含 `text/plain`

---

### E2E-GW-004 命中域名且合法 key 放行

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Bearer key_basic_001
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. `status == 200`
2. 包含 upstream 正常响应内容

---

### E2E-GW-005 并发超限拒绝

**步骤**
1. 发起第 1 个长连接请求（占槽）
2. 不结束第 1 个请求，发起第 2 个同 key 请求

**第 2 个请求断言字段**
1. `status == 429`
2. `headers["content-type"]` 包含 `text/plain`
3. `body == error_response.message`

---

### E2E-GW-006 释放后恢复

**步骤**
1. 完成 E2E-GW-005 中第 1 个请求
2. 再发同 key 请求

**断言字段**
1. `status == 200`

---

### E2E-GW-007 分布式后端异常 fail-open（/acquire 503）

**步骤**
1. 让 counter-service acquire 返回 503（例如停 Redis）
2. 发 gateway 请求

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Bearer key_basic_001
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. `status == 200`（请求继续）
2. 非 429

---

### E2E-GW-008 分布式后端异常 fail-open（/acquire 非 200）

**步骤**
1. 让 `/acquire` 响应为 500
2. 发 gateway 请求

**断言字段**
1. `status == 200`
2. 非 429

---

### E2E-GW-009 分布式后端异常 fail-open（/acquire 响应体非法 JSON）

**步骤**
1. 让 `/acquire` 返回 `200 + body=not-json`
2. 发 gateway 请求

**断言字段**
1. `status == 200`
2. 非 429

---

### E2E-GW-010 stream 请求自动注入 include_usage

**Request**
```http
POST /v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Bearer <JWT_WITH_UID_123>
Content-Type: application/json

{"stream":true,"messages":[{"role":"user","content":"hi"}]}
```

**断言字段**
1. 上游收到请求体包含：`$.stream_options.include_usage == true`
2. 如果原本有 `Content-Length`，转发请求不应带旧值（可通过上游日志/抓包断言）

---

### E2E-GW-011 多副本全局并发一致性

**步骤**
1. 将 ingressgateway 扩容到 2 副本
2. 强制两路流量命中不同副本（可基于源地址/连接复用策略）
3. 同时发起 2 个 `key_basic_001` 请求（limit=1）

**断言字段**
1. 仅 1 个请求成功（200）
2. 另 1 个请求被拒绝（429）

---

### E2E-GW-012 配置热更新：阈值变更

**步骤**
1. 初始 `max_concurrent=1`，验证第二个并发被拒绝
2. 调用 `/config` 更新为 `max_concurrent=2`
3. 再做 2 并发请求

**断言字段**
1. 更新后 2 并发均可通过（200）

---

### E2E-GW-013 配置热更新：删除 key

**步骤**
1. 删除 `llm-demo.local + key_basic_001`
2. 继续发该 key 请求

**断言字段**
1. gateway 响应 `429`

---

## 5. MET 层用例（指标与标签）

### E2E-MET-001 非流式 usage 统计

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Bearer <JWT_WITH_UID_sfe-platform>
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段（抓取 ingress `/stats/prometheus`）**
1. `llm_prompt_tokens_total{domain="llm-demo.local",uid="sfe-platform"}` 递增
2. `llm_completion_tokens_total{domain="llm-demo.local",uid="sfe-platform"}` 递增

---

### E2E-MET-002 SSE usage 统计

**步骤**
1. 发起 stream 请求并确保 SSE 最终帧带 usage
2. 请求完成后抓指标

**断言字段**
1. prompt/completion 指标有增量

---

### E2E-MET-003 SSE 非法帧解析错误

**步骤**
1. upstream 返回 `text/event-stream`，包含非法 JSON 帧：`data: {not json}`
2. 抓指标

**断言字段**
1. `llm_stream_parse_errors_total{domain="llm-demo.local",uid="123"}` 递增

---

### E2E-MET-004 uid 解析失败时禁用 token 统计

**Request**
```http
POST /mock/v1/chat/completions HTTP/1.1
Host: llm-demo.local
Authorization: Bearer abc
Content-Type: application/json

{"messages":[{"role":"user","content":"hello"}]}
```

**断言字段**
1. 请求仍按限流逻辑执行（不因统计失败中断主流程）
2. 不新增该请求对应的 token 指标序列

---

### E2E-MET-005 Istio 1.13 标签提取

**前置**：mesh config 已包含 `extraStatTags: [domain, uid]`。

**断言字段**
1. 指标以标签形式暴露 `domain`、`uid`，而非仅体现在 `__name__`
2. 指标名保持：
   - `llm_prompt_tokens_total`
   - `llm_completion_tokens_total`
   - `llm_stream_parse_errors_total`

---

## 6. 实现建议（编码阶段）

1. 目录建议
   - `e2e/cs/`：直接打 counter-service
   - `e2e/gw/`：经 gateway 打全链路
   - `e2e/met/`：指标断言
2. 公共能力
   - HTTP client 封装（重试、超时、trace id）
   - 配置初始化 helper（PUT /config）
   - 指标抓取与解析 helper（Prometheus text parser）
3. 执行策略
   - PR: 仅跑 P0（<10 分钟）
   - Nightly: 跑 P0 + P1

## 7. 最小可交付顺序

1. 先实现 `CS` 层 001~019（稳定且可快速反馈）
2. 再实现 `GW` 层 001~010（主功能）
3. 最后实现 `GW-011+` 与 `MET`（依赖集群与观测）
