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

#### 2.1.2 API Key 识别
- 从 HTTP 请求头中提取 API Key
- 默认 Header 名称：`X-API-Key`
- 支持自定义 Header 名称配置
- 处理缺失 API Key 的情况

#### 2.1.3 真实并发数限制
- 限制的是同一时刻正在处理中的请求数量（in-flight requests），而非 QPS 或速率
- 工作原理：
  - 请求到达时：检查该 API Key 当前 in-flight 计数，若 < max_concurrent 则放行并计数 +1，否则拒绝
  - 请求完成时（收到响应或连接断开）：计数 -1
- 不同的 API Key 可配置不同的最大并发数
- 实时跟踪每个 API Key 的当前 in-flight 请求数

#### 2.1.4 超限处理
- 当某个 API Key 达到最大并发限制时，拒绝新请求
- 返回 HTTP 429 (Too Many Requests) 状态码
- 返回清晰的错误消息，包含限流信息


#### 2.1.5 LLM Token 用量统计

功能目标：精确统计每个 API Key 消耗的输入和输出 Token 数量，用于计费、配额管理及可观测性展示。

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

api_key_header: "X-API-Key"  # 可选，默认为 X-API-Key

rate_limits:
  - api_key: "key_basic_001"
    max_concurrent: 10       # 最大同时处理请求数

  - api_key: "key_premium_001"
    max_concurrent: 50

  - api_key: "default"       # 未匹配到的 API Key 使用此默认配置
    max_concurrent: 5

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

### 2.3 配置热更新

#### 2.3.1 需求描述
- 修改配置后无需重启 Envoy 实例即可生效
- 允许短暂延迟，最长不超过 1 分钟
- 支持的热更新场景：
  - 新增 API Key 及其并发限制
  - 修改已有 API Key 的 max_concurrent 阈值
  - 删除 API Key（移除后该 Key 回退到 default 配置）
  - 新增或移除拦截域名

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
  "message": "缺失或无效的 API Key"
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

### 3.3 可观测性
- 记录关键操作日志
- 暴露限流指标（Prometheus 格式）：
  - 每个 API Key 的当前并发数
  - 限流拒绝次数
  - 请求处理延迟
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
- 请求到达时：原子读取计数，若 < max_concurrent 则原子 +1 并放行，否则拒绝
- 请求完成时：原子 -1（通过 OnHttpResponseHeaders 或 OnLog 回调）
- 需处理异常断开场景，确保计数器不会泄漏

### 4.2 配置热更新实现
- 配置存储为可原子替换的引用
- OnPluginStart 接收新配置时：
  1. 解析并校验新配置
  2. 对已有 API Key：保留当前 in-flight 计数，仅更新 max_concurrent
  3. 对新增 API Key：初始化计数器为 0
  4. 对删除的 API Key：等待 in-flight 归零后清理，期间使用 default 配置
- 配置解析失败时记录错误日志，继续使用旧配置

### 4.3 WebAssembly 集成
- 使用 Proxy-Wasm Go SDK
- 实现必要的生命周期回调：
  - OnPluginStart：加载/重新加载配置
  - OnHttpRequestHeaders：检查并发数，决定放行或拒绝
  - OnHttpResponseHeaders / OnLog：释放并发计数

### 4.4 状态管理
- 使用共享内存存储限流状态
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
- 支持 Istio 1.14+ 版本

### 5.3 配置示例
提供完整的 Kubernetes/Istio 部署 YAML 示例

## 6. 测试要求

### 6.1 单元测试
- 并发计数器逻辑测试
- 配置解析测试
- 配置热更新逻辑测试

### 6.2 集成测试
- 在真实 Envoy 环境中测试
- 验证限流准确性
- 压力测试验证性能

### 6.3 测试场景
- 正常请求流量（并发数未达上限）
- 超限场景（并发数达到上限，新请求被拒绝）
- 并发释放（请求完成后计数器正确递减，新请求可放行）
- 无效 API Key
- 高并发场景
- 配置热更新：
  - 运行中新增 API Key，验证新 Key 立即生效
  - 运行中修改 max_concurrent，验证新阈值生效
  - 运行中删除 API Key，验证回退到 default 配置
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
4. 使用文档
5. 测试报告

## 8. 后续优化方向

- 支持分布式并发限制（跨多个 Envoy 实例共享计数，如通过 Redis）
- 支持更多限流策略（QPS 速率限制、滑动窗口等）
- 集成外部配置中心（如 etcd）实现更灵活的动态配置
- 提供管理 API 查询实时并发状态
