# rate-limiter-wasm e2e 测试需求文档

> 日期：2026-03-24
> 状态：Draft v1（用于后续自动化实现）
> 约束：本阶段仅产出需求与用例蓝图，不修改业务代码

## 1. 目标

为项目建立可落地的端到端（e2e）测试体系，覆盖以下真实运行路径：

1. Counter Service + Redis 服务链路
2. Istio Ingress + WASM 插件 + Counter Service + Redis + Upstream 的完整数据面链路
3. Token 统计与指标暴露链路（含 Istio 1.13 `extraStatTags`）

测试目标不是“验证设计理想状态”，而是优先验证**当前实现语义**，并把与 PRD 历史表述不一致之处显式化。

## 2. 实现基线（当前代码语义）

以下行为作为 e2e 验收基线：

1. 仅命中 `domains` 的请求进入鉴权/限流/统计；未命中域名直接 bypass。
2. 分布式模式为异步 callout：请求先 pause，等待 `/acquire` 回调。
3. Counter Service 返回非 200 或返回体不可解析时，插件采用 **fail-open**（恢复请求继续）。
4. 只有 `/acquire` 返回 `200 + {allowed:false}` 时，插件本地拒绝请求。
5. 插件拒绝响应格式为：`HTTP 429 + text/plain; charset=utf-8 + error_response.message`。
6. Counter Service `Acquire` 的业务拒绝（如 `limit_exceeded`、`config_not_found`、`api_key_disabled`）默认是 `HTTP 200 + allowed=false`。
7. Counter Service `Acquire` 仅 Redis 不可用时返回 `503 + reason=redis_unavailable`。
8. Redis 配置匹配优先级：精确 > wildcard > 全局。
9. 采用 ZSET 租约模型，需验证 TTL 到期后自动恢复并发槽位。
10. Token 统计支持：
   - 非流式 JSON usage 提取
   - SSE 增量 usage 提取
   - `stream: true` 时请求体自动注入 `stream_options.include_usage=true`
11. 指标内部命名格式：
   - `llm.prompt_tokens_total.;domain=.=<domain>;.;uid=.=<uid>;.;`
   - `llm.completion_tokens_total.;domain=.=<domain>;.;uid=.=<uid>;.;`
   - `llm.stream_parse_errors_total.;domain=.=<domain>;.;uid=.=<uid>;.;`

## 3. 测试范围

### 3.1 必测（P0）

1. 配置 CRUD 与 Acquire/Release 主流程
2. 并发超限、释放、幂等、TTL 自动恢复
3. 域名命中与 bypass 边界
4. Authorization 合法/非法分支
5. 分布式后端异常 fail-open 行为
6. token 统计（非流式 + SSE）
7. Istio 1.13 指标标签（`domain`/`uid`）

### 3.2 应测（P1）

1. 多 ingress 副本全局并发一致性
2. 配置热更新（新增/修改/删除）
3. SSE 非法帧解析错误计数
4. 指标 key 上限溢出到 `__other__`

### 3.3 暂不纳入首批自动化（P2）

1. 大规模压测（>10k QPS）
2. 长时间 soak test（>12h）
3. 混沌注入（网络抖动、随机丢包）

## 4. 环境要求

1. Kubernetes + Istio 1.13.x
2. 至少 2 个 ingressgateway 副本（用于全局并发场景）
3. 已部署：
   - `deploy/istio/redis.yaml`
   - `deploy/istio/counter-service-deployment.yaml`
   - `deploy/istio/rate-limiter-envoyfilter.yaml`
   - `deploy/nginx/*` + `simple/istio.yaml`
4. Mesh Config 已合并：

```yaml
defaultConfig:
  extraStatTags:
    - domain
    - uid
```

## 5. 测试数据规范

1. Domain：
   - `llm-demo.local`（主路径）
   - `api.example.com`（精确）
   - `foo.example.com`（wildcard）
   - `unmatched.local`（bypass）
2. API Key：
   - `key_basic_001`（limit=1）
   - `key_premium_001`（limit=2）
   - `key_disabled_001`（enabled=false）
   - `key_missing_001`（无配置）
3. JWT：
   - 合法 uid：`{"uid":"123"}`
   - 合法 uid：`{"uid":"sfe-platform"}`
   - 非法：非 JWT token（如 `abc`）

## 6. 通过标准

1. P0 用例通过率 100%
2. P1 用例通过率 >= 95%
3. 不允许出现并发槽位永久泄漏（TTL 恢复必须稳定）
4. 指标验收以 `/stats/prometheus` 实际输出为准

## 7. 交付物（后续实现阶段）

1. `e2e` 自动化用例代码（按分层组织）
2. 测试环境初始化脚本（导入配置、种子数据）
3. CI 执行入口（P0 PR 必跑，P1 夜跑）
4. 测试报告模板（失败样本请求/响应与指标快照）

## 8. 与历史文档差异（已对齐当前实现）

1. 分布式异常处理：由“本地降级限流”统一为 **fail-open**。
2. 插件拒绝响应：由 JSON 示例统一为 **text/plain**。
3. Acquire 拒绝状态码：业务拒绝以 `HTTP 200 + allowed=false` 为准（非 429/404）。
4. 服务命名统一使用 `counter-service`。

## 9. 下一步

下一份文档《e2e 用例分解清单》给出逐条请求报文与断言字段，作为编码实现蓝图。
