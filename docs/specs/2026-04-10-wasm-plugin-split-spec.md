# WASM 插件拆分 Spec

## 1. 摘要

当前仓库把“按实际并发数限流”和“LLM token usage stats”实现为同一个 Proxy-WASM 插件。该设计将现有单插件拆分为两个独立部署、独立配置、独立构建的 WASM 插件：

- `rate-limiter`：只负责按实际并发数限流
- `token-stats`：只负责 token usage stats

两个插件采用**单仓库、多 Go module** 方式组织，共享能力下沉到 `shared` 模块。`counter-service` 明确归属于 `rate-limiter` 模块，`token-stats` 不依赖也不包含 `counter-service`。

## 2. 背景与问题

当前代码将两类职责放在同一个插件中：

1. 请求阶段按 `Authorization: Bearer <api_key>` 做域名命中判断、鉴权、并发 acquire
2. 响应阶段做非 SSE / SSE 的 usage 解析、流式请求体注入、Prometheus 指标上报

这种结构的问题：

- **职责耦合**：限流和统计逻辑在同一插件中修改，影响面大
- **构建产物膨胀**：token stats 插件不需要分布式限流与 `counter-service` 相关能力，却被一并打包
- **部署不灵活**：无法独立启停某一能力
- **演进成本高**：限流和统计变更必须一起发布

## 3. 目标

### 3.1 功能目标

1. 将现有单一 WASM 插件拆分为两个独立插件
2. 两个插件分别独立配置、独立构建、独立部署
3. 保留当前行为语义：
   - 域名未命中时完全 bypass
   - 限流插件继续支持实际并发数限流
   - token-stats 插件继续支持非 SSE / SSE usage 解析
   - token-stats 插件继续支持自动注入 `stream_options.include_usage`
4. 保留 `uid` 标签能力：token-stats 继续从 JWT 中提取 `uid`

### 3.2 工程目标

1. 代码按模块边界清晰拆分
2. 共享能力只保留在 `shared` 中
3. `counter-service` 只存在于 `rate-limiter` 模块
4. 旧的单插件模式不再继续维护，迁移到双插件模式

## 4. 非目标

1. 不保留单插件兼容构建产物
2. 不在本次拆分中改变限流策略
3. 不在本次拆分中新增新的 token 指标类型
4. 不在本次拆分中引入新的外部配置中心

## 5. 最终仓库结构

本次拆分完成后，仓库目录以用户确认的结构为准：

```text
rate-limiter-wasm/
├── docs/
│   └── specs/
│
├── shared/
│   ├── go.mod
│   └── auth/
│       ├── bearer.go
│       └── jwt.go
│   └── matcher/
│       └── domain.go
│
├── rate-limiter/
│   ├── go.mod
│   ├── cmd/
│   │   ├── wasm/
│   │   │   └── main.go
│   │   └── counter-service/
│   │       └── main.go
│   └── internal/
│       ├── config/
│       ├── limiter/
│       ├── store/
│       └── plugin/
│
└── token-stats/
    ├── go.mod
    ├── cmd/
    │   └── wasm/
    │       └── main.go
    └── internal/
        ├── config/
        └── plugin/
```

## 6. 模块职责边界

### 6.1 `shared` 模块

`shared` 只放两个插件都可能复用、且职责稳定的公共能力。

#### `shared/auth/bearer.go`
职责：
- 解析 `Authorization: Bearer <token>`
- 复用当前 Bearer 解析逻辑与错误语义

导出接口建议：
- `ParseBearerToken(headerValue string) (string, error)`

#### `shared/auth/jwt.go`
职责：
- 从 Bearer token 对应的 JWT payload 中提取 `uid`
- 复用当前 token-stats 对 `uid` 的解析规则与约束

导出接口建议：
- `ParseUIDFromJWT(authorizationHeader string) (string, error)`

说明：
- 当前只有 `token-stats` 使用 `uid` 解析
- 仍将其放入 `shared/auth`，作为统一的认证解析工具集合
- `rate-limiter` 当前不需要依赖 `jwt.go`

#### `shared/matcher/domain.go`
职责：
- 实现域名精确匹配与通配符匹配
- 复用当前 `domains` 匹配逻辑

导出接口建议：
- `NewDomainMatcher(patterns []string) (*DomainMatcher, error)`
- `(*DomainMatcher) Match(host string) bool`

### 6.2 `rate-limiter` 模块

`rate-limiter` 只负责并发限流链路。

包含职责：
- 域名命中判断
- Bearer token 提取
- API Key 到并发阈值映射
- 本地并发限流
- 分布式并发限流
- `counter-service` acquire / release 调用
- fail-open 与恢复逻辑
- 限流错误响应

包含目录：
- `cmd/wasm`：限流插件入口
- `cmd/counter-service`：并发计数服务入口
- `internal/config`：限流插件配置
- `internal/limiter`：本地 / 分布式 limiter
- `internal/store`：与 `counter-service` 交互的存储客户端
- `internal/plugin`：限流插件实现

明确不包含：
- token usage 解析
- SSE 响应解析
- `stream_options.include_usage` 注入
- token metrics 上报

### 6.3 `token-stats` 模块

`token-stats` 只负责 usage 统计链路。

包含职责：
- 域名命中判断
- Bearer token 提取
- JWT `uid` 提取
- 流式请求 `stream_options.include_usage` 注入
- 非 SSE 响应 usage 解析
- SSE 响应 usage 解析
- token metrics 上报

包含目录：
- `cmd/wasm`：统计插件入口
- `internal/config`：统计插件配置
- `internal/plugin`：统计插件实现

明确不包含：
- limiter
- store
- distributed limit
- `counter-service`

## 7. 依赖规则

### 7.1 模块依赖方向

依赖关系必须保持单向：

```text
rate-limiter  ──┐
                ├──> shared
token-stats   ──┘
```

约束：
- `rate-limiter` 和 `token-stats` 都只允许依赖 `shared`
- `token-stats` 不允许依赖 `rate-limiter/internal/...`
- `rate-limiter` 不允许依赖 `token-stats/internal/...`
- `counter-service` 只存在于 `rate-limiter/cmd/counter-service`

### 7.2 Go module 组织方式

- `shared/go.mod`
- `rate-limiter/go.mod`
- `token-stats/go.mod`

开发阶段由 `rate-limiter` 和 `token-stats` 在各自 `go.mod` 中通过 `replace ../shared` 引用本地 `shared` 模块。

## 8. 插件行为设计

### 8.1 `rate-limiter` 插件行为

#### 请求进入时

1. 读取 `:authority`
2. 使用 `shared/matcher` 判断是否命中 `domains`
3. 未命中则直接 `continue`
4. 命中后读取 `authorization`
5. 使用 `shared/auth.ParseBearerToken()` 提取 API Key
6. 按配置执行限流判定：
   - 本地 limiter；或
   - 分布式 acquire（通过 `counter-service`）
7. 超限则返回本地错误响应
8. 放行则记录 release 所需状态

#### 请求结束时

- 本地 limiter：释放并发计数
- 分布式 limiter：向 `counter-service` 发送 release

#### 分布式失败时

- 保持当前 fail-open 语义
- 后端错误不阻断主请求链路
- 保留恢复逻辑

### 8.2 `token-stats` 插件行为

#### 请求进入时

1. 读取 `:authority`
2. 判断是否命中 `domains`
3. 未命中则完全 bypass
4. 命中后读取 `authorization`
5. 使用 `shared/auth.ParseBearerToken()` 提取 Bearer token
6. 使用 `shared/auth.ParseUIDFromJWT()` 提取 `uid`
7. 若配置开启 `inject_stream_usage`，在请求体阶段处理流式注入

#### 请求体处理

当满足以下条件时：
- `token_statistics.enabled = true`
- `inject_stream_usage = true`
- 请求 body 为 JSON
- 检测到 `stream: true`
- 当前未包含 `stream_options.include_usage = true`

则自动将请求体改写为：

```json
{
  "stream": true,
  "stream_options": {
    "include_usage": true
  }
}
```

并移除 `content-length`。

#### 响应处理

- 非 SSE：缓冲到 `endOfStream=true` 后解析 `usage.prompt_tokens` / `usage.completion_tokens`
- SSE：按 data 帧增量解析 usage
- 解析失败时累加 parse error 指标

#### 指标标签

保留现有核心标签语义：
- `domain`
- `uid`

#### 不参与的行为

`token-stats` 不进行：
- 限流
- 分布式 acquire / release
- `counter-service` 调用

## 9. 配置拆分

### 9.1 `rate-limiter` 配置

建议配置结构：

```yaml
domains:
  - "api.example.com"
  - "*.service.example.com"

rate_limits:
  - api_key: "key_basic_001"
    max_concurrent: 10
  - api_key: "key_premium_001"
    max_concurrent: 50

distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "counter-service.default.svc.cluster.local"
    timeout_ms: 1000
    acquire_path: "/acquire"
    release_path: "/release"
    lease_ttl_ms: 30000

error_response:
  status_code: 429
  message: "Rate limit exceeded for API key"
```

### 9.2 `token-stats` 配置

建议配置结构：

```yaml
domains:
  - "api.example.com"
  - "*.service.example.com"

token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
```

### 9.3 配置原则

- 两个插件各自维护独立配置
- 两边都保留自己的 `domains`
- 运维需要保证两个插件的 `domains` 一致
- `rate-limiter` 不包含 `token_statistics`
- `token-stats` 不包含 `rate_limits`、`distributed_limit`

## 10. 构建与产物

### 10.1 目标产物

拆分后至少产出：

- `dist/rate-limiter.wasm`
- `dist/token-stats.wasm`
- `dist/counter-service`（若保留二进制构建产物）

### 10.2 构建入口

- `rate-limiter/cmd/wasm/main.go` -> `rate-limiter.wasm`
- `token-stats/cmd/wasm/main.go` -> `token-stats.wasm`
- `rate-limiter/cmd/counter-service/main.go` -> `counter-service`

### 10.3 构建原则

- 两个 WASM 插件独立编译
- `token-stats.wasm` 不应包含 limiter / store / counter-service 相关实现
- `counter-service` 只随 `rate-limiter` 模块演进

## 11. 部署模型

### 11.1 独立部署

两个插件通过两个独立的 EnvoyFilter 部署：

- `rate-limiter` EnvoyFilter
- `token-stats` EnvoyFilter

两者：
- 配置独立
- 版本独立
- 可单独发布

### 11.2 插件关系

用户已明确要求：
- 两个插件独立部署
- 配置互不引用
- 不是单插件内的两个模式

因此部署上应视为两个并列的 HTTP WASM 插件。

## 12. 迁移策略

### 12.1 迁移目标

从“单插件 + 单份配置”迁移到：

- 一个限流插件
- 一个 token stats 插件
- 两份独立配置

### 12.2 迁移步骤

1. 从现有代码中抽离共享能力到 `shared`
2. 创建 `rate-limiter` module，并迁移限流相关实现
3. 创建 `token-stats` module，并迁移统计相关实现
4. 将 `counter-service` 移动到 `rate-limiter/cmd/counter-service`
5. 拆分原配置结构为两份配置
6. 更新构建脚本，输出两个 `.wasm`
7. 更新部署 YAML，拆分为两个 EnvoyFilter
8. 删除旧单插件入口与旧单插件配置方式

## 13. 风险与约束

### 13.1 `domains` 配置漂移

风险：
- 两个插件独立配置后，`domains` 可能不一致

影响：
- 限流命中但统计未命中，或反之

缓解：
- 在部署模板层复用同一份 `domains` 数据源
- 在文档中明确要求运维保持一致

### 13.2 shared 变更影响面

风险：
- `shared/auth` 或 `shared/matcher` 的变更同时影响两个模块

缓解：
- 保持 `shared` 足够小
- 不把业务逻辑继续下沉到 `shared`
- 为 `shared` 增加独立测试

### 13.3 拆分后的测试覆盖

风险：
- 迁移过程中逻辑遗漏

缓解：
- 限流与 stats 分别补齐模块级测试
- 为 `shared` 增加单元测试
- 保留关键行为回归测试

## 14. 验收标准

### 14.1 结构验收

1. 仓库目录与本 spec 中的最终目录一致
2. `counter-service` 仅位于 `rate-limiter/cmd/counter-service`
3. `token-stats` 模块中不存在 limiter/store/counter-service 代码

### 14.2 功能验收

1. `rate-limiter` 单独部署时，限流功能正常
2. `token-stats` 单独部署时，token 指标正常
3. 两者同时部署时，行为与拆分前一致
4. token-stats 对非 SSE 与 SSE 的 usage 解析行为保持一致
5. token-stats 继续支持 `uid` 标签

### 14.3 构建验收

1. 两个插件可以独立编译为 `.wasm`
2. `token-stats.wasm` 体积显著小于原单插件产物
3. `rate-limiter.wasm` 可继续配合 `counter-service` 工作

## 15. 后续实现范围

本 spec 之后的实现工作应至少覆盖：

1. 新建三个 Go module
2. 抽离 `shared/auth` 与 `shared/matcher`
3. 将现有限流逻辑迁移到 `rate-limiter`
4. 将现有 token stats 逻辑迁移到 `token-stats`
5. 调整构建脚本与输出路径
6. 更新部署与测试

---

本 spec 为拆分方案的基线文档。后续实现必须以本 spec 中确认的模块边界和目录结构为准。