# Counter Service 分布式限流在 WASM 内真实可用：设计说明（方案 1）
日期：2026-03-17

## 背景 / 问题陈述

当前仓库目标是在 Envoy/Istio 的 Proxy-WASM 插件中实现分布式并发限流：插件通过异步 HTTP callout 调用 Counter Service，Counter Service 再使用 Redis（Lua 脚本）实现配置查找 + 并发计数的原子操作。

目前 `bash ./build.sh`（TinyGo + `-target=wasi -scheduler=none`）编译失败，错误为：

- `attempted to start a goroutine without a scheduler`

根因定位：

1. WASM 构建产物中包含了 [internal/store/client.go](internal/store/client.go) 的真实 HTTP 客户端实现，其使用：
   - `go func()`（如 [internal/store/client.go:94](internal/store/client.go#L94)）
   - `net/http`、`context.WithTimeout`（这些在 TinyGo 标准库实现里也会间接启动 goroutine）

2. 但 TinyGo 构建参数为 `-scheduler=none`（见 [build.sh](build.sh)），在该配置下任何 goroutine 都是不允许的。

同时，从 Proxy-WASM 运行时能力角度看，WASM 内进行传统 socket 形式的 `net/http.Client` 也不符合 Envoy Proxy-WASM 的典型运行模型；正确方式是通过 host call `proxywasm.DispatchHttpCall()` 进行异步 HTTP callout。

## 目标

1. Counter Service 分布式模式在 WASM 内“真实可用”，并且唯一分布式调用路径为：
   - 插件侧 `proxywasm.DispatchHttpCall()` → Counter Service `/acquire`、`/release`
2. `bash ./build.sh` 可成功编译（不再引入 goroutine / net/http 标准库传输实现）
3. Counter Service API 契约与请求体字段固定为 **选项 A**：
   - Acquire 请求体：`{ "domain": "...", "api_key": "...", "ttl_ms": ... }`
4. 插件错误处理语义固定为：
   - `DispatchHttpCall` 返回 err：直接 reject
   - 已成功 dispatch 后若超时 / 非 200 / 响应解析失败：直接 fail-open 透传请求，不做本次限流判断

## 非目标

- 不在本次设计中实现“WASM 内同步 DistributedStore interface 的真实后端”。
- 不尝试通过修改 TinyGo scheduler 来支持 goroutine。
- 不在 WASM 内使用 `net/http` 来访问 Counter Service。

## 设计方案（方案 1：推荐并已获确认）

### 总体原则

- **Counter Service 分布式模式只走插件层异步 callout**。
- 将 `internal/store/client.go` 回退为 WASM 安全的占位实现（不依赖 `net/http`、不使用 goroutine），用于：
  - 非 counter_service 的潜在同步后端（未来扩展），或
  - 维持代码结构/接口存在，但不影响 WASM 构建。

### 组件与职责

1. **WASM 插件（internal/plugin/root.go）**
   - 从 `:authority` 提取并归一化 domain。
   - 解析 `Authorization: Bearer <api_key>`。
   - 当启用 counter_service 分布式模式时：
     - Acquire：`DispatchHttpCall` → `POST /acquire` 并 `ActionPause`。
     - Release：`OnHttpStreamDone` best-effort `DispatchHttpCall` → `POST /release`。
   - 错误处理与降级（按最新确认语义）：
     - **DispatchHttpCall 返回 err（无法 dispatch）**：直接 reject（不 fallback）。
     - **已成功 dispatch 但超时/状态码非 200/响应解析失败**：直接 `ResumeHttpRequest()` 继续透传请求，**不做任何限流判断**（fail-open）。

2. **Counter Service（cmd/counter-service + internal/counter-service/**）
   - `/acquire`：读取 Redis 动态配置 + 并发计数（Lua 原子脚本），返回 `AcquireResult`。
   - `/release`：根据 lease 释放并发计数（Lua）。
   - `/config`、`/configs`：配置管理（Redis 存储），用于运维动态修改。

3. **Redis（Lua 脚本）**
   - 使用 `domain + api_key` 维度查找配置，支持通配符 fallback（范围定义：**只做一级 parent 通配 + 全局通配**）：
     1) 精确匹配 `config:{domain}:{api_key}`
     2) 父域名通配 `config:*.{parent}:{api_key}`（例：`a.b.c` 的 parent 通配为 `*.b.c`）
     3) 全局通配 `config:*:{api_key}`
   - lease 机制避免槽位泄漏。

### 数据流（WASM 插件）

#### Acquire（OnHttpRequestHeaders）

关键实现点已存在于 [internal/plugin/root.go:185-226](internal/plugin/root.go#L185-L226)：

1. 读取 `:authority`，并做 domain normalize。
2. 构造 JSON body（字段固定为选项 A）：

```json
{ "domain": "api.example.com", "api_key": "key001", "ttl_ms": 30000 }
```

3. `proxywasm.DispatchHttpCall(cs.Cluster, headers, body, nil, timeout, onAcquireResponse)`
4. 返回 `types.ActionPause`

#### Acquire 回调（onAcquireResponse）

关键语义已存在于 [internal/plugin/root.go:544-619](internal/plugin/root.go#L544-L619)，但本次设计将错误处理更新为 **fail-open（仅限已成功 dispatch 后的失败）**：

- status != 200：**直接 Resume 透传请求**（不 fallback 到本地 limiter）
- 响应体解析失败：**直接 Resume 透传请求**（不 fallback）
- 解析响应体：
  - Allowed=false：拒绝（本地 send response）
  - Allowed=true：保存 lease_id → Resume

注意：此处允许 Counter Service 返回更丰富的 `AcquireResult`（reason/message/max_concurrent/current_count/tier），插件可记录日志，但判定以 Allowed 为主。

#### Release（OnHttpStreamDone）

关键实现点已存在于 [internal/plugin/root.go:418-455](internal/plugin/root.go#L418-L455)：

- 如果持有 `distributedLeaseID`：best-effort callout `/release`，忽略响应。

### internal/store 的定位与调整

当前 `internal/store/client.go` 通过 `net/http` 实现了真实 HTTP acquire/release，并使用 goroutine 异步 release，这在 TinyGo `-scheduler=none` 下不可用，并且与 Proxy-WASM 运行时模型不一致。

本方案要求：

- `internal/store/client.go` 必须改为 **WASM 安全**：
  - 不 import `net/http`
  - 不使用 `go func()`
  - 不使用 `context.WithTimeout`（避免标准库内部 goroutine）
- 在 counter_service 分布式模式下，插件不会调用 `store.NewClient(...)`，这一点已由 [internal/plugin/root.go:652-664](internal/plugin/root.go#L652-L664) 明确。

### API 契约

#### /acquire

Acquire 请求（选项 A）：
- `POST /acquire`
- JSON: `{domain, api_key, ttl_ms}`

Acquire 响应（**状态码契约：总是 200 表达策略结果**）：

| 场景 | HTTP 状态码 | 响应体关键字段 | 插件行为 |
|---|---:|---|---|
| 策略允许 | 200 | `allowed=true`, `lease_id` MUST 非空 | 保存 `lease_id`，Resume |
| 策略拒绝（超限/未配置/禁用/配置非法） | 200 | `allowed=false`, `reason` MUST 非空 | reject（不 fallback） |
| 基础设施失败（Redis 不可用/内部错误） | 503/5xx | 可返回 `allowed=false` + `reason`（可选） | `status!=200` → fail-open Resume |

- `allowed=true`：`lease_id` **必须**非空且全局唯一；若无法生成 lease，应返回 5xx（触发插件 fail-open），禁止 200 + allowed=true + 空 lease_id。

> 说明：这是为了与插件最新语义对齐。插件的 `onAcquireResponse`（见 [internal/plugin/root.go:544-619](internal/plugin/root.go#L544-L619)）在已成功 dispatch 后若收到 `status != 200`，会直接 `ResumeHttpRequest()` 透传，而不是 fallback 到本地 limiter；因此“策略拒绝”必须继续使用 `HTTP 200 + allowed=false` 表达，避免被误判为基础设施故障并放行。

#### /release

Release 请求：
- `POST /release`
- JSON: `{api_key, lease_id}`（插件当前实现如此）
  - `api_key`：客户端层面会发送；服务端可选择忽略（用于日志关联）。
  - `lease_id`：MUST 非空。

Release 响应（最小可测试契约）：

| 场景 | HTTP 状态码 | 响应体建议 | 语义 |
|---|---:|---|---|
| 成功释放 | 200 | `{released:true}`（可选 `current_count`） | 成功 |
| lease 不存在/已过期 | 200 | `{released:false, reason:"lease_not_found"}` | **幂等**（重复释放不会报错） |
| Redis 不可用/内部错误 | 503/5xx | `{released:false, reason:"redis_unavailable"}`（可选） | 基础设施失败 |

> 说明：插件忽略 release 响应（best-effort），但契约清晰有助于 Counter Service 自测、监控与排障；如需强校验 api_key 与 lease 绑定关系需在后续单独设计。

## 错误处理 / 降级策略

### 插件侧（WASM）

为了避免“策略拒绝”被误判为基础设施故障并错误放行，本规范采用 **/acquire 总是 200 表达策略结果** 的契约（详见上文 `### API 契约`）。同时，插件错误处理按用户最新确认语义更新为“**仅 dispatch 后失败时 fail-open**”：

- **DispatchHttpCall 直接失败**（例如 cluster 配置错误、无法 dispatch）：**直接 reject**（不 fallback，不放行）。
- **Counter Service 返回 `status != 200`**（视为基础设施失败）：**直接 `ResumeHttpRequest()` 透传请求**，不做本次限流判断。
- **响应体 JSON 解析失败**：**直接 `ResumeHttpRequest()` 透传请求**，不做本次限流判断。
- **`status==200` 且 `allowed==false`**：**reject**（返回配置的 `error_response`）。
- **`status==200` 且 `allowed==true`**：保存 `lease_id`，然后 `ResumeHttpRequest()`。

### Counter Service 侧

- Redis 网络错误/不可用：返回 `HTTP 503`（或其它 5xx），插件会按最新语义 **fail-open 透传请求**。
- 策略拒绝（未配置/禁用/超限/配置非法）：返回 `HTTP 200` + `allowed=false` + `reason`。

## 验证标准（Definition of Done）

1. `bash ./build.sh` 成功产出 `dist/rate-limiter.wasm`
2. `go test ./... -count=1` 通过
3. 在启用 `distributed_limit.enabled=true` 且 `backend=counter_service` 时：
   - 插件能发出 `/acquire` callout
   - Counter Service 能从 Redis 命中配置并返回 `HTTP 200` + `allowed=true|false`
   - 请求结束触发 `/release` best-effort callout（仅当 acquire 成功返回 lease_id 时）
4. Redis/Counter Service 不可用（返回 5xx 或超时）时，插件按最新语义直接 fail-open 透传请求；仅在 `DispatchHttpCall` 自身返回 err 时直接拒绝

建议测试覆盖点（用于防回归）：

插件侧（WASM）：
- `DispatchHttpCall` 返回 err 必须走拒绝路径（不 fallback，不放行）。
- `status==200 && allowed==false` 必须走拒绝路径（不放行）。
- `status!=200` 必须直接 `ResumeHttpRequest()` 放行（不 fallback）。
- 响应体解析失败必须直接 `ResumeHttpRequest()` 放行（不 fallback）。
- acquire 失败/拒绝的请求不应触发 release callout。

Counter Service 侧（契约测试）：
- 超限/未配置/禁用等“策略拒绝”必须返回 `HTTP 200` + `allowed=false`（禁止 4xx/429，否则插件会按 fail-open 语义放行）。
- Redis 不可用必须返回 5xx（建议 503），用于触发插件 fail-open。
- /release 对 lease_not_found 必须幂等（200 + released=false）。
## 实施影响面（预计修改）

- 修改 [internal/store/client.go](internal/store/client.go) 与 [internal/store/client_test.go](internal/store/client_test.go)：移除真实 HTTP 实现，改为占位并调整测试断言。
- 修改 Counter Service `/acquire` handler：将“策略拒绝（超限/未配置/禁用/配置非法）”从 4xx/429 调整为 **HTTP 200 + allowed=false**（与插件最新 `status!=200` → fail-open 行为对齐）。
- 可能需要更新/补充插件侧测试，验证 async callout 分支在编译与逻辑上保持正确。

---

该文档为设计说明（spec），用于后续生成实现计划与执行步骤。
