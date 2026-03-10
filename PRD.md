# Istio Envoy WebAssembly 并发限流插件需求文档

## 1. 项目概述

### 1.1 项目目标
开发一个基于 WebAssembly 的 Envoy 插件，用于在 Istio 服务网格中实现基于 API Key 的并发请求限流功能。

### 1.2 技术栈
- 开发语言：Go
- 运行环境：Istio Envoy (WebAssembly)
- 限流模型：真实并发数限制（in-flight request counting）

## 2. 功能需求

### 2.1 核心功能

#### 2.1.1 域名拦截
- 插件应能够拦截特定域名的 HTTP/HTTPS 请求
- 支持配置多个目标域名
- 支持精确匹配和通配符匹配（如 `*.example.com`）
- 当请求域名未命中 `domains` 配置时，插件必须完全 bypass：不执行 API Key 校验、不执行并发限流、不执行 Token 统计，也不改写请求或响应

#### 2.1.2 API Key 识别
- 从 HTTP 请求头 `Authorization` 中提取 API Key
- 首版仅支持 `Authorization: Bearer <api_key>` 格式
- 当 `Authorization` 头缺失、为空、格式非法，或 Bearer token 为空时，直接拒绝请求，不进入 `default` 配置
- API Key 提取成功但未在 `rate_limits` 中显式配置时，同样直接拒绝请求，不进入 `default` 配置
- `default` 配置仅用于文档保留的兼容扩展位；首版实现中不得用于兜底放行未显式配置的 API Key

#### 2.1.3 真实并发数限制（支持分布式共享计数）
- 限制的是同一时刻正在处理中的请求数量（in-flight requests），而非 QPS 或速率
- 首版必须支持跨多个 Envoy 实例共享并发计数，确保同一 API Key 在多副本部署下执行全局并发限制，而非单实例局部限制
- 分布式计数能力采用可插拔存储设计，但首版必须交付可用的 Redis 后端实现作为默认分布式共享计数方案；实现上仍不应将存储层与插件主逻辑强耦合，便于后续替换为其他外部状态存储
- 工作原理：
  - 请求到达时：优先检查该 API Key 的全局 in-flight 计数，若 < max_concurrent 则放行并计数 +1，否则拒绝
  - 请求完成时（收到响应或连接断开）：全局计数 -1
  - 当分布式计数存储不可用时，插件自动降级为单实例本地 in-flight 计数模式，保证服务可用性；同时记录降级状态与恢复状态，用于运维观测
- 不同的 API Key 可配置不同的最大并发数
- 实时跟踪每个 API Key 的当前 in-flight 请求数
- 插件需避免因重试、超时、连接中断或重复回调导致计数泄漏或重复扣减

#### 2.1.4 超限处理
- 当某个 API Key 达到最大并发限制时，拒绝新请求
- 返回 HTTP 429 (Too Many Requests) 状态码
- 返回清晰的错误消息，包含限流信息


#### 2.1.5 LLM Token 用量统计

功能目标：精确统计每个 API Key 消耗的输入和输出 Token 数量，用于计费、配额管理及可观测性展示。

适用范围：仅对命中 `domains` 配置的请求启用 Token 统计；未命中域名的请求完全 bypass，不参与任何 Token 统计逻辑。

统计来源：优先从 LLM 服务提供商返回的响应数据中提取 usage 字段，确保数据与提供商计费账单一致，避免本地估算误差。

支持模式：

1. 非流式响应：

- 解析 HTTP 响应体中的 JSON 数据。
- 提取字段：usage.prompt_tokens（输入Token）和 usage.completion_tokens（输出Token）。

2. 流式响应 (SSE, Server-Sent Events)：
- 自动注入参数：检测到请求为流式（如参数含 stream: true）时，插件应自动在请求体中注入 stream_options: {include_usage: true}（遵循 OpenAI API 规范），确保服务端在流结束前返回统计信息。
- 事件解析：拦截 SSE 响应流，识别并解析最后一个包含 usage 字段的 data: 事件。
- 连接中断处理：若流式连接异常中断导致未收到 usage 事件，该次请求标记为“统计失败”或仅记录输入Token（若有估算值），不记录输出Token。

数据关联：统计的 Token 数量需与发起请求的 API Key、请求域名、模型名称 进行关联绑定。


### 2.2 配置管理

#### 2.2.1 插件配置项
```yaml
# 示例配置结构
domains:
  - "api.example.com"
  - "*.service.example.com"

# API Key 从 Authorization: Bearer <api_key> 中提取

rate_limits:
  - api_key: "key_basic_001"
    max_concurrent: 10       # 最大同时处理请求数

  - api_key: "key_premium_001"
    max_concurrent: 50

  - api_key: "default"       # 仅保留为兼容扩展位，首版不用于兜底放行未显式配置的 API Key
    max_concurrent: 5

distributed_limit:
  enabled: true
  backend: "redis"            # 首版必须交付 redis 后端实现，作为默认分布式共享计数方案
  fallback_to_local: true      # 分布式存储异常时降级为本地限流
  key_prefix: "ratelimit:inflight"
  redis:
    address: "redis.default.svc.cluster.local:6379"
    password: ""
    db: 0
    dial_timeout_ms: 100
    read_timeout_ms: 100
    write_timeout_ms: 100

error_response:
  status_code: 429
  message: "Rate limit exceeded for API key"

token_statistics:
  enabled: true                   # 是否启用 Token 统计
  inject_stream_usage: true       # 是否自动为流式请求注入 include_usage 参数

```

#### 2.2.2 配置加载
- 支持从 Envoy 配置文件加载
- 支持通过 Istio EnvoyFilter 注入配置
- 分布式限流配置需支持声明共享存储后端类型及其连接参数
- 首版必须提供可运行的 Redis 后端配置与部署示例，后续其他后端应复用统一配置抽象

### 2.3 配置热更新

#### 2.3.1 需求描述
- 修改配置后无需重启 Envoy 实例即可生效
- 允许短暂延迟，最长不超过 1 分钟
- 支持的热更新场景：
  - 新增 API Key 及其并发限制
  - 修改已有 API Key 的 max_concurrent 阈值
  - 删除 API Key（移除后该 Key 直接拒绝，不再由 `default` 配置兜底）
  - 新增或移除拦截域名
  - 更新分布式限流后端配置（如 Redis 地址、超时、key 前缀、开关项）

#### 2.3.2 实现机制
- 利用 Istio EnvoyFilter 配置变更触发 Envoy 的 xDS 推送
- 插件通过 `OnPluginStart` 回调接收新配置并重新解析
- 配置更新时的行为：
  - 已在处理中的请求（in-flight）不受影响，继续按旧配置执行直到完成
  - 新到达的请求立即使用新配置
  - 并发计数器在配置切换时平滑过渡：保留当前 in-flight 计数，仅更新阈值上限

#### 2.3.3 配置更新流程
```
1. 运维修改 EnvoyFilter YAML 并 kubectl apply
2. Istio 控制面检测到变更，通过 xDS 推送新配置到 Envoy
3. Envoy 调用插件的 OnPluginStart，传入新的 plugin_configuration
4. 插件解析新配置，原子替换内部配置引用
5. 新请求使用新配置，已有请求不受影响
```

#### 2.3.4 配置更新约束
- 配置格式错误时，保留旧配置并记录错误日志，不影响服务
- 配置更新期间不应出现请求丢失或误拒绝

### 2.4 错误响应格式

#### 2.4.1 超限响应
```json
{
  "error": "rate_limit_exceeded",
  "message": "并发请求数已达到限制",
  "api_key": "key_***_001",  // 部分隐藏
  "limit": 10,
  "retry_after": 1  // 建议重试时间（秒）
}
```

#### 2.4.2 无效 API Key 响应
```json
{
  "error": "invalid_api_key",
  "message": "Authorization 头缺失、格式非法，或其中的 API Key 未在限流配置中注册"
}
```

## 3. 非功能需求

### 3.1 性能要求
- 插件处理延迟 < 1ms (P99)
- 支持高并发场景（10000+ QPS）
- 内存占用合理（每个 API Key < 1KB）

### 3.2 可靠性要求
- 插件崩溃不应影响 Envoy 主进程
- 配置错误应有明确的错误提示
- 限流状态应准确，避免误判
- 分布式共享计数存储异常时，插件必须自动降级到单实例本地限流模式，并在共享存储恢复后可平滑恢复到分布式模式

### 3.3 可观测性
- 记录关键操作日志
- 暴露限流指标（Prometheus 格式）：
  - 每个 API Key 的当前并发数
  - 限流拒绝次数
  - 请求处理延迟
  - distributed_limit_degrade_total (Counter): 分布式限流降级次数
  - distributed_limit_recover_total (Counter): 分布式限流恢复次数
  - distributed_limit_backend_errors_total (Counter): 分布式后端访问失败次数
- 暴露 LLM Token 指标 (新增)：
  - llm_prompt_tokens_total (Counter): 输入 Token 累计消耗。
    - 标签：api_key (脱敏), model, domain。
  - llm_completion_tokens_total (Counter): 输出 Token 累计消耗。
    - 标签：api_key (脱敏), model, domain。
  - llm_stream_parse_errors_total (Counter): 流式响应解析失败次数（用于监控 SSE 解析稳定性）

### 3.4 安全性
- API Key 在日志中应部分隐藏
- 防止恶意请求耗尽资源
- 配置验证，防止配置注入攻击

## 4. 技术实现要点

### 4.1 并发计数器实现
- 每个 API Key 维护一个 in-flight 计数器
- 计数器实现需同时支持本地计数模式与分布式共享计数模式，并通过统一接口屏蔽底层存储差异
- 请求到达时：优先读取全局共享计数，若 < max_concurrent 则执行原子 +1 并放行，否则拒绝
- 请求完成时：执行原子 -1（通过 OnHttpResponseHeaders 或 OnLog 回调）
- 分布式后端需提供原子自增、自减、过期保护或租约续期能力，避免请求异常退出后长期占用计数
- 当分布式后端不可达或操作超时时，按配置自动切换为本地计数模式，并持续探测共享后端恢复状态
- 需处理异常断开场景，确保计数器不会泄漏

### 4.2 配置热更新实现
- 配置存储为可原子替换的引用
- OnPluginStart 接收新配置时：
  1. 解析并校验新配置
  2. 对已有 API Key：保留当前 in-flight 计数，仅更新 max_concurrent
  3. 对新增 API Key：初始化计数器为 0
  4. 对删除的 API Key：等待 in-flight 归零后清理；清理完成后该 Key 的新请求直接拒绝，不再使用 `default` 配置兜底
  5. 对分布式存储配置：复用统一存储接口平滑切换连接与参数，避免影响已在处理中的请求
- 配置解析失败时记录错误日志，继续使用旧配置

### 4.3 WebAssembly 集成
- 使用 Proxy-Wasm Go SDK
- 实现必要的生命周期回调：
  - OnPluginStart：加载/重新加载配置
  - OnHttpRequestHeaders：对命中 `domains` 的请求解析 `Authorization: Bearer <api_key>`，完成 API Key 校验、并发检查，并决定放行或拒绝
  - OnHttpResponseHeaders / OnLog：释放并发计数
- 未命中 `domains` 的请求在请求头阶段直接 bypass，不执行 Authorization 解析、并发限流或 Token 统计
- 与外部分布式存储交互时需控制单次请求的超时与失败影响范围，避免阻塞 Envoy 主处理链路

### 4.4 状态管理
- 本地 worker 内状态使用共享内存存储限流状态
- 跨 Envoy 实例的全局并发状态通过可插拔外部存储维护，首版必须提供 Redis 后端实现
- 需要区分本地视角计数与全局视角计数，避免监控与调试混淆
- 考虑多 worker 场景的数据同步

### 4.5 Token 统计实现细节

#### 4.5.1 非流式响应处理

- 在 OnHttpResponseHeaders 检查 Content-Type 为 application/json。
- 在 OnHttpResponseBody 中等待 endOfStream=true，合并所有 Body 片段。
- 使用轻量级 JSON 解析器提取 usage 对象，避免引入重型 JSON 库以控制 WASM 体积。

#### 4.5.2 流式响应 (SSE) 处理

- 请求阶段干预：
  - 在 OnHttpRequestBody 中读取请求体。
  - 若检测到 stream: true 且配置了 inject_stream_usage: true，修改请求体 JSON，添加 stream_options 字段。
  - 更新 HTTP Header 中的 Content-Length（若存在且能计算）。
- 响应阶段解析：
  - 在 OnHttpResponseHeaders 检查 Content-Type 为 text/event-stream。
  - 在 OnHttpResponseBody 中增量解析 SSE 事件：
    - 识别格式：data: {...}。
    - 缓存或实时检查每个 JSON payload，查找包含 usage 字段的事件。
    - 通常 usage 出现在最后一个 data: [DONE] 之前的帧中。
  - 捕获到 usage 数据后，立即更新 Prometheus 计数器。

#### 4.5.3 性能与内存优化

- SSE 解析采用增量读取模式，仅保留必要的解析状态，避免在内存中缓存整个响应体。
- 对于超大 Token 用量的响应（如 >10k tokens），确保解析逻辑不会导致 WASM 内存溢出（OOM）。

## 5. 部署要求

### 5.1 编译产物
- 生成 `.wasm` 文件
- 文件大小尽量优化（< 5MB）

### 5.2 Istio 集成
- 通过 EnvoyFilter 资源部署
- 支持 Istio 1.13+ 版本

### 5.3 配置示例
提供完整的 Kubernetes/Istio 部署 YAML 示例

## 6. 测试要求

### 6.1 单元测试
- 并发计数器逻辑测试
- 分布式计数存储抽象测试（首版必须包含 Redis 后端契约测试）
- 配置解析测试
- 配置热更新逻辑测试

### 6.2 集成测试
- 在真实 Envoy 环境中测试
- 验证限流准确性
- 验证多实例部署下的全局并发限制准确性
- 首版必须包含基于 Redis 后端的端到端集成测试
- 验证分布式后端异常时的本地降级行为与恢复行为
- 压力测试验证性能

### 6.3 测试场景
- 正常请求流量（命中域名，且 `Authorization: Bearer <api_key>` 合法，并发数未达上限）
- 超限场景（并发数达到上限，新请求被拒绝）
- 并发释放（请求完成后计数器正确递减，新请求可放行）
- 无效 API Key：
  - 命中域名但缺失 `Authorization` 头
  - 命中域名但 `Authorization` 头格式非法
  - 命中域名且 Bearer token 为空
  - 命中域名且 Bearer token 未在 `rate_limits` 中注册
- 域名未命中场景：即使请求缺失 `Authorization`，也应完全 bypass，不执行限流与 Token 统计
- 高并发场景
- 多实例共享限流场景：
  - 同一 API Key 的请求分别落到多个 Envoy 实例，验证使用全局并发计数后总并发不超过阈值
  - 多个 API Key 并发访问，验证全局共享计数互不串扰
- 分布式后端异常场景：
  - Redis 不可用或超时，验证插件自动降级为本地限流模式
  - Redis 恢复后，验证插件可平滑恢复为分布式限流模式
- 配置热更新：
  - 运行中新增 API Key，验证新 Key 立即生效
  - 运行中修改 max_concurrent，验证新阈值生效
  - 运行中删除 API Key，验证该 Key 后续请求直接拒绝，不再回退到 `default` 配置
  - 运行中修改分布式后端配置，验证新配置生效且不影响存量请求
  - 提交无效配置，验证旧配置不受影响
- 异常断开场景（连接中断后计数器正确释放）
- token 统计测试
  - 非流式 Token 统计测试：
    - 发送非流式请求，验证 Prometheus 指标是否正确记录输入/输出 Token。
    - 验证缺失 usage 字段的响应不会导致插件崩溃。
  - 流式 Token 统计测试：
    - 发送流式请求，验证请求体是否被自动注入 stream_options。
    - 验证能否正确从 SSE 流的末尾提取 Token 并记录指标。
    - 模拟流式传输中途断开（RST_STREAM），验证插件状态是否正常释放，指标是否正确处理（如不计入输出 Token）。
  - 多模型测试：
    - 发送不同模型（如 gpt-4 和 gpt-3.5-turbo）的请求，验证指标 Label 是否正确区分。

## 7. 交付物

1. Go 源代码
2. 编译后的 .wasm 文件
3. 部署配置示例（EnvoyFilter YAML）
4. Redis 后端配置与部署示例
5. 使用文档
6. 测试报告

## 8. 后续优化方向

- 支持更多限流策略（QPS 速率限制、滑动窗口等）
- 集成外部配置中心（如 etcd）实现更灵活的动态配置
- 提供管理 API 查询实时并发状态
- 扩展更多分布式共享计数后端（如 etcd、Consul 或专用限流服务）
