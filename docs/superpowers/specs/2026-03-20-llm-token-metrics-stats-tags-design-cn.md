# LLM token 指标 stats_tags 重设计
## 摘要
本设计将 LLM token 指标更新为兼容 Istio 1.13 的 stats 标签提取模型。WASM 插件使用 Istio 1.13 已知如何通过 `extraStatTags` 提取的分隔符模式，将 `domain` 和 `uid` 编码到内部 Envoy 统计名称中，而不是依赖于 `EnvoyFilter` BOOTSTRAP 补丁中的自定义 `stats_config.stats_tags` 正则表达式。
目标保持不变：
- 保持最终的 Prometheus 指标名称不变
- 将 `domain` 和 `uid` 作为一等标签暴露
- 保留标签值中的 `-` 和 `.`
- 移除 `ServiceMonitor.metricRelabelings`
目标 Prometheus 输出：
```promql
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```
关于标签值的说明：`domain` 是规范化的主机（小写，通过 `normalizeHost()` 去除端口）。`uid` 经过 `sanitizeMetricValue()` 处理，保留了 `-`、`.`、`_`，仅替换结构上不安全的字符（`;`、`=`、`|`、空白符、控制字符、非 ASCII 字符）。这些标签不是原始请求值，但对于典型的 ASCII 输入，它们接近原始值。
## 问题陈述
原始实现在 WASM 插件中使用如下名称定义指标：
```text
llm_prompt_tokens_total;domain=<domain>;uid=<uid>;
```
然后 Prometheus 使用 `metricRelabelings` 从 `__name__` 中恢复 `domain` 和 `uid`。
这对于实际值会出错，因为 Envoy 统计名称净化会在 Prometheus 看到指标名称之前，将非 `[a-zA-Z0-9_]` 的字符标准化。实际上：
- `-` 变为 `_`
- `.` 变为 `_`
- 分隔符字符如 `;` 和 `=` 也会变为 `_`
这导致了原始设计中的两个失败：
1. 当原始 `domain` / `uid` 值包含 `-` 或 `.` 时，无法重构。
2. Prometheus 中的正则提取很脆弱，可能会产生伪影。
第一次重设计试图通过自定义点分统计格式和通过 `EnvoyFilter` BOOTSTRAP 补丁配置的显式 Envoy `stats_config.stats_tags` 正则来解决这个问题。
然而，在 Istio 1.13.5 上的真实集群验证显示了一个重要的兼容性约束：
1. 通过 `EnvoyFilter` BOOTSTRAP 配置的所需自定义 `stats_config.stats_tags` 正则并未按预期生效。
2. Istio 1.13 转而通过 mesh config `defaultConfig.extraStatTags` 暴露统计标签提取功能。
3. 对于 `domain` 和 `uid` 等标签，Istio 1.13 使用内置提取模式，而不是 `EnvoyFilter` 中尝试的自定义正则。
所以问题不仅仅是“在 Prometheus 之前提取标签”。实际需求是：
- 使用与 Istio 1.13 内置标签提取行为相匹配的内部统计名称形态
- 在 mesh config 中配置 `extraStatTags`
- 避免在 `EnvoyFilter` 中依赖自定义 BOOTSTRAP 正则
## 设计目标
1. 保持最终的 Prometheus 指标名称不变：
   - `llm_prompt_tokens_total`
   - `llm_completion_tokens_total`
   - `llm_stream_parse_errors_total`
2. 将 `domain` 和 `uid` 作为一等 Prometheus 标签暴露。
3. 保留标签值中的 `-` 和 `.` 字符。
4. 移除对 `ServiceMonitor.metricRelabelings` 从 `__name__` 恢复标签的依赖。
5. 保持插件端的更改仅集中于指标命名，而不更改 token 统计语义。
6. 使部署模型与已验证的 Istio 1.13.5 行为相匹配。
## 非目标
1. 不更改从请求派生 `uid` 的方式。
2. 不更改现有的 `sanitizeMetricValue` 或 `normalizeHost` 行为。标签值反映这些函数的输出，而非原始请求值。
3. 不更改 token 统计启用、计数器或核算行为。
4. 不扩展到 `domain` 和 `uid` 之外的新指标维度。
5. 不提供自定义 Prometheus 端点。
6. 不尝试为所有 Istio 版本保留通用的自定义正则 Envoy bootstrap 解决方案。
## 选定的方案
使用与 Istio 1.13 针对 `domain` 和 `uid` 的内置统计标签提取兼容的内部统计名称形态，并通过 mesh config `defaultConfig.extraStatTags` 配置这些标签。
**规范性兼容规则：** 目标行为同时取决于以下两点：
1. 插件以此确切结构形态发送统计名称：
   `llm.<metric>domain=.=<domain>;.;uid=.=<uid>;.;`
2. 目标网关环境的 Istio mesh config 在 `defaultConfig.extraStatTags` 中列出 `domain` 和 `uid`
如果缺少任一条件，则目标 `domain` / `uid` 标签对于 Istio 1.13.5 而言不被视为正确实现。
### 内部统计名称格式
```text
llm.<metric>domain=.=<domain>;.;uid=.=<uid>;.;
```
示例：
```text
llm.prompt_tokens_totaldomain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
llm.completion_tokens_totaldomain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
llm.stream_parse_errors_totaldomain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
```
配合 mesh config：
```yaml
defaultConfig:
  extraStatTags:
    - domain
    - uid
```
Istio 1.13 使用其内置标签处理功能从这些内部统计名称中提取 `domain` 和 `uid`，其余暴露的指标名称与现有的 Prometheus 名称保持一致。
### 为什么采用此方案
与原始 Prometheus 重新标记方法相比，这将标签提取提前到了 Prometheus 暴露之前的 Envoy/Istio 统计层。
与放弃的自定义 `stats_tags` 正则设计相比，此方案与在真实 Istio 1.13.5 集群中验证的实际行为相匹配。
与引入可逆的自定义编码方案相比，这保持了指标名称的可读性，同时在正常情况下保留了 `domain` 和 `uid` 中重要的字符。
## 考虑过的替代方案
### 替代方案 A：保留 Prometheus metricRelabelings
拒绝，因为它仍然过晚地恢复标签，此时 Envoy 统计名称标准化已经丢失了重要的字符信息。
### 替代方案 B：自定义点分统计名称加 EnvoyFilter 中的自定义 BOOTSTRAP 正则
拒绝的形态示例：
```text
llm.<metric>.__host0domain__.<domain>.__user0id__.<uid>
```
拒绝原因是 Istio 1.13.5 上的真实集群验证表明，通过 `EnvoyFilter` BOOTSTRAP 配置的自定义 `stats_config.stats_tags` 正则并未按需运作。保留此设计将使仓库与实际部署路径脱节。
### 替代方案 C：在文档中保留双路径设计
拒绝，因为项目的目标环境明确为 Istio 1.13.5。让主设计文档同时描述“通用”自定义正则设计和 Istio 1.13 兼容设计会增加混乱，而无助于当前的部署目标。
## 详细设计
### 1. 插件指标命名
指标定义逻辑必须生成符合 Istio 1.13 兼容格式的名称：
- `llm.%sdomain=.=<domain>;.;uid=.=<uid>;.;`
一种可接受的实现是：
```go
func buildMetricName(metric, domain, uid string) string {
    return fmt.Sprintf("llm.%sdomain=.=%s;.;uid=.=%s;.;", metric, domain, uid)
}
```
只要所有三个计数器系列发出相同的可观察统计名称格式，任何等效的实现都是可以接受的。
受影响的指标系列：
- `prompt_tokens_total`
- `completion_tokens_total`
- `stream_parse_errors_total`
此更改仅影响指标命名，不影响 token 累积或请求流行为。
### 2. 净化行为
现有的 `sanitizeMetricValue` 行为仍然适用，并且在本设计中，它是兼容性契约的一部分，而非任意的实现细节。
它保留：
- `.`
- `-`
- `_`
它替换明显不安全的结构性字符，如：
- `;`
- `=`
- `|`
- 空白符和控制字符
- 非 ASCII 字符
这很重要，因为内部统计名称契约取决于分隔符结构的完整性：
```text
domain=.=<domain>;.;uid=.=<uid>;.;
```
因此，只有在能保持 Istio 1.13 标签提取识别该结构的能力时，才接受净化器的更改。特别是，必须不允许净化后的值注入或保留会破坏 `domain` / `uid` 段边界的分隔符字符。
### 3. Istio 1.13 标签提取模型
部署模型从“通过 `EnvoyFilter` BOOTSTRAP 配置自定义正则”更改为“通过 mesh config `extraStatTags` 配置标签名称”。
所需的 mesh config 形态：
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: istio
  namespace: istio-system
data:
  mesh: |-
    defaultConfig:
      extraStatTags:
        - domain
        - uid
```
在此项目验证过的 Istio 1.13.5 环境中：
- `extraStatTags` 导致 bootstrap 配置包含 `domain` 和 `uid` 标签提取器
- 这些提取器使用 Istio 提供的内置正则行为
- 在 `EnvoyFilter` BOOTSTRAP 中尝试的自定义正则值在此版本中不是可靠的控制点
因此，设计明确依赖于：
1. 插件指标名称遵循预期的内部分隔符形态
2. mesh config 在 `extraStatTags` 中列出 `domain` 和 `uid`
### Mesh config 示例与仓库一致性
为了使此设计可从仓库构件复现，仓库应包含或更新一个 Istio mesh 配置示例，显示：
```yaml
defaultConfig:
  extraStatTags:
    - domain
    - uid
```
此示例可以位于专门的部署示例文件中，或在明确记录的操作员说明中，但如果期望部署示例对 Istio 1.13 是自包含的，它必须作为仓库中维护的构件存在。
### EnvoyFilter 部署示例
仓库 `deploy/istio/rate-limiter-envoyfilter.yaml` 不应再包含试图定义自定义 `stats_config.stats_tags` 正则的 `BOOTSTRAP` 补丁。
该示例应仅关注：
- 用于 counter-service 上游的 `CLUSTER` 补丁
- 用于 WASM 过滤器插入的 `HTTP_FILTER` 补丁
- 内联插件配置
这使 EnvoyFilter 示例与 Istio 1.13 实际需要的内容保持一致。
### 5. Prometheus ServiceMonitor
`deploy/istio/prometheus-servicemonitor.yaml` 不再需要 `metricRelabelings`。
一旦插件指标名称格式和 Istio 标签提取对齐，Prometheus 将直接从 Envoy 的 `/stats/prometheus` 输出接收最终的指标名称和标签。
### 6. 预期最终指标
预期的 Prometheus 指标保持为：
```text
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```
## 测试策略
### 插件测试
更新 `internal/plugin/token_stats_test.go` 以断言新的内部统计名称格式：
- `llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;`
- `llm.completion_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;`
- `llm.stream_parse_errors_totaldomain=.=api.example.com;.;uid=.=123;.;`
同时更新边界情况测试，使其继续验证：
- 包含 `.` 的域得到保留
- 包含 `-` 的 UID 得到保留
- 包含 `.` 的 UID 得到保留
- 溢出到 `__other__` 在新的统计名称格式下仍然有效
### 显式弃用检查
仓库不应再将以下任何内容作为 Istio 1.13 的主要路径：
- `__host0domain__`
- `__user0id__`
- `EnvoyFilter` BOOTSTRAP `stats_config.stats_tags` 自定义正则
- `ServiceMonitor.metricRelabelings`
这些只能作为已拒绝或历史方法被提及。测试、部署示例和实现说明不得将其视为活动架构。
### 部署配置检查
- `deploy/istio/rate-limiter-envoyfilter.yaml` 在移除 BOOTSTRAP 补丁后应为有效 YAML
- `deploy/istio/prometheus-servicemonitor.yaml` 在没有 `metricRelabelings` 的情况下应保持有效 YAML
### 实时集群验证
对于 Istio 1.13 部署，应按以下顺序进行验证：
1. 确保 mesh config 包含：
   - `extraStatTags: [domain, uid]`
2. 确认 ingress proxy bootstrap 包含为 `domain` 和 `uid` 生成的标签提取器
3. 部署更新后的 WASM 插件
4. 检查 ingress Envoy 的 `/stats/prometheus` 输出
5. 确认最终指标暴露：
   - 未更改的指标名称
   - 具有预期值的 `domain` 和 `uid` 标签
6. 如果在推出期间实时环境中仍存在临时的兼容性重新标记，则仅在验证新路径后将其移除。它不是目标架构的一部分。
## 迁移 / 推出说明
1. 本设计是针对此项目中已验证的 Istio 1.13.5 行为编写的。不要假设相同的控制点或提取路径自动适用于其他 Istio 版本。
2. 在生产仪表板中依赖新的指标标签之前，应用包含 `extraStatTags: [domain, uid]` 的 mesh config。
3. 不要依赖 Istio 1.13 中旧的 `EnvoyFilter` BOOTSTRAP 自定义正则补丁。
4. 仅在验证新指标存在于实时集群中后，再移除 `ServiceMonitor.metricRelabelings`。
5. 仪表板和告警查询可以继续使用相同的指标名称，同时切换到原生 `domain` / `uid` 标签。
## 必须保持一致的文件
- `internal/plugin/root.go`
- `internal/plugin/token_stats_test.go`
- `deploy/istio/rate-limiter-envoyfilter.yaml`
- `deploy/istio/prometheus-servicemonitor.yaml`
- 定义 `defaultConfig.extraStatTags: [domain, uid]` 的 Istio mesh config 示例或部署说明
- `docs/superpowers/specs/2026-03-20-llm-token-metrics-stats-tags-design.md`
如果其中任何一项仍反映已放弃的 `__host0domain__ / __user0id__ + 自定义正则` 设计，则仓库将变得内部不一致。
