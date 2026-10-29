# rate-limiter-wasm E2E 用例与断言

本清单替代原2026-03-24用例中的旧拒绝语义。依据[测试要求](2026-03-24-e2e-test-requirements.md)和[当前限流契约](../specs/rate-limiting-contract.md)，由 `scripts/k8s_e2e.py --rate-limiter-only` 执行。

## Counter Service

| 用例 | 操作 | 必须满足的断言 |
| --- | --- | --- |
| CS-001 | GET /health | 200，不要求Redis在线 |
| CS-002 | PUT /config，domain=API.EXAMPLE.COM，key=known，limit=1，enabled=true | 200；Redis配置键为小写域名和SHA256后缀 |
| CS-003 | GET /config?domain=API.EXAMPLE.COM&api_key=known | 200，domain小写，有api_key_hash，无api_key/tier |
| CS-004 | GET /configs?cursor=0&limit=2 循环分页 | configs为数组，没有api_key；遍历到cursor=0才结束 |
| CS-005 | DELETE /config?domain=API.EXAMPLE.COM&api_key=known | 200；后续GET404，但acquire正常放行 |
| CS-010 | 有额度时acquire | 200、allowed=true、lease_id非空、current_count=1 |
| CS-011 | 占满key额度后再acquire，另有可用域名规则 | 200、allowed=false、reason=limit_exceeded；不回退；拒绝响应不包含max_concurrent/current_count |
| CS-012 | 所有候选不存在 | 200、对象为allowed=true；无lease_id，无租约写入 |
| CS-013 | key禁用 | 继续key通配和域名规则；全部禁用则同CS-012 |
| CS-014 | release有效租约 | 200、released=true，释放额度 |
| CS-015 | 重复release | 200、released=false、reason=lease_not_found |
| CS-016 | 租约过期后再acquire | 清理过期ZSET成员，current_count重新为1 |
| CS-017 | 精确/直接父域/全局key规则并存 | 按该顺序选择；禁用逐条跳过，之后才查域名规则 |
| CS-018 | key与空key均无规则 | 不创建通配共享计数池，直接无租约放行 |
| CS-019 | Redis实际停机 | 800ms内得到完整503、allowed=false、reason=redis_unavailable |
| CS-020 | hash错误、空值/null/非字符串，enabled缺失/null/非布尔，或携带tier | 400；不新增配置、不改变原字段或更新时间 |
| CS-021 | 将具体key查询/列表对象原样PUT | 400；不得创建空key域名规则；补原始key及匹配hash可正常更新 |
| CS-022 | description先写、再省略、再覆盖、再空串 | 分别创建、保留、覆盖、删除，Redis不写明文key或tier |
| CS-023 | Redis enabled缺失/非法，或启用阈值缺失/非正整数 | acquire500/invalid_config；不回退、不计数；GET和列表也500，不显示成false |

配置请求示例：

```http
PUT /config HTTP/1.1
Content-Type: application/json

{"domain":"api.example.com","api_key":"known","max_concurrent":1,"enabled":true,"description":"限流测试"}
```

具体key查询对象没有明文key。补原始key后才允许使用其api_key_hash写入；域名规则的hash为SHA256空字符串，省略key仍可通过校验。

## 真实网关

| 用例 | 操作 | 必须满足的断言 |
| --- | --- | --- |
| GW-001 | 未匹配插件domains | 到达上游，业务200 |
| GW-002/003 | 缺失或非法Authorization，且没有域名规则 | 业务200，无租约；有域名规则时改按其额度判定 |
| GW-004 | 有效key且有额度 | 到达上游；结束后release |
| GW-005/006 | 副本0持有key长流，副本1同key再请求 | 长流跨多个TTL持续续约；副本1超限429；流结束后恢复 |
| GW-007/008/009 | acquire503、其他非200、非法JSON | 内部故障可观测，业务继续200，不创建租约 |
| GW-010 | 无启用规则 | 正常放行，无续约/释放，非统计用例 |
| GW-011 | 具体key与域名规则并存 | key优先；key超限不借域名额度；未知key仍可使用域名池 |
| GW-012 | 阈值1改2，随后删除key | 新请求立即读新值；删除后无规则放行，在途流不被中断 |
| GW-013 | 禁用key，再禁用域名规则 | 先回退域名；全部禁用时无租约放行 |
| GW-014 | 通配规则匹配两个实际域名 | 各自独立计数，不共享跨域池 |
| GW-015 | 直接破坏Redis enabled/阈值 | fixture记录内部500/invalid_config，网关上游200，无回退计数 |
| GW-016 | acquire旧api_key_disabled/config_not_found原因、缺allowed、未知原因、超大响应 | 全部放行，不冒充明确超限 |
| GW-017 | renew/release超时、503或异常体 | 原业务响应完整；租约过期后后续acquire可恢复 |
| GW-018 | Wasm加载、初始化、trap、无下载源冷启动 | 两副本业务200，保留ECDS默认透传及故障证据 |

## 运行约束

初始化与故障恢复后均使用真实占满额度的探针确认限流插件已经生效；不得使用缺配置或enabled=false触发429作为激活条件。

故障注入只在脚本创建的隔离命名空间进行，结束后删除测试资源并比较原有资源配置。完整运行方法、镜像与产物要求见[执行指南](../k8s-e2e.md)。

## 已废弃项

原MET-*统计用例不属于当前限流验收。token-stats已废弃、停止维护，现有代码和旧完整测试入口暂时保留；不新增或修复统计功能。
