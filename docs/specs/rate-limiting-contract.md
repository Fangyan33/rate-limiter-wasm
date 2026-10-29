# 当前限流契约

本文说明 `rate-limiter`、Counter Service 和 Redis 当前共同遵循的行为。部署命令见 [README](../../README.md)，隔离集群测试见 [Kubernetes E2E](../k8s-e2e.md)。历史设计文件保留用于追溯，不作为当前实施步骤。

`token-stats` 已废弃、停止维护，后续将完整移除；代码、构建和既有部署清单暂时保留，不属于限流功能或本次验收范围。

## 匹配与放行

网关仅处理 `domains` 命中的请求。具体 API Key 从 Bearer 中提取，缺失或格式错误时视为没有可用 key，不承担身份认证职责。

按以下顺序逐条查询候选规则：

1. 精确域名 + 具体 API Key。
2. 直接父域通配 + 具体 API Key。
3. 全局通配 `*` + 具体 API Key。
4. 精确域名 + 空 key。
5. 直接父域通配 + 空 key。
6. 全局通配 `*` + 空 key。

没有有效 key 时只执行后三步。缺失记录和 `enabled=false` 的记录跳过，继续下一步。禁用具体 key 后可匹配同 key 通配或域名规则；全部候选不存在或被禁用时直接放行，不占用任何额度。

启用规则一旦命中就使用该规则的额度，达到上限时拒绝，不继续回退。具体 key 额度按实际请求域名 + key 隔离；空 key 额度按实际域名共享。通配配置不建立跨域共享池，两类额度不叠加。

Redis 通配查找只使用直接父域，例如 `a.b.example.com` 查询 `*.b.example.com` 和 `*`，不会递归查询 `*.example.com`。网关 `domains` 的后缀通配是进入插件的条件，与 Redis 查找层级不同。

本地静态模式同样先选具体 key，再选空 key，每个实际域名独立计数；规则为空或未匹配时放行。静态模式没有 Redis 的 enabled 字段，删除配置项即可停止该规则。本地配置重新加载会创建新的计数器，不保证在途计数平滑保留。

## 配置写入与查询

`PUT /config` 示例：

```json
{
  "domain": "api.example.com",
  "api_key": "key001",
  "max_concurrent": 2,
  "enabled": true,
  "description": "接口并发额度"
}
```

- `domain` 在 CRUD 和 acquire 中去首尾空格并转为小写，不能为空。
- `api_key` 可省略或为空字符串，表示域名规则；非空值按原始字节计算摘要，不自动去空格或改大小写。
- `max_concurrent` 必须为正整数，`enabled` 必须显式提供布尔值。缺失或非法启用状态不能被默认为禁用。
- `api_key_hash` 可选；出现时必须严格等于 `SHA256(api_key)` 的小写十六进制摘要。空值、null、类型错误或不匹配返回 400。
- `description` 省略时保留旧描述，非空字符串覆盖，显式空字符串清空；更新原子完成。
- `tier` 已删除，写入携带该字段返回 400。可选字段出现时不接受 null。
- 校验失败不执行 Redis 写入，也不修改更新时间。项目未上线，不提供旧数据迁移或兼容分支。

`GET /config?domain=...&api_key=...` 和 `GET /configs?cursor=0&limit=100` 的配置对象只返回 `domain`、`api_key_hash`、`max_concurrent`、`enabled`、可选 description 和更新时间，完全没有 `api_key` 或 tier 字段。单项查询和删除仍通过明文 api_key 查询参数定位，服务端计算摘要。

列表只支持 cursor/limit，不支持 domain 筛选。必须遍历到 cursor="0"；空页返回 configs=[]，不代表本轮结束。

不能直接把具体 key 的查询对象 PUT 回去：没有原始 key 时，其 hash 与空 key 不一致，必须返回 400；编辑时需要补入原始 key。域名规则的 hash 与空 key 一致，省略 key 可通过校验。

## Redis 数据结构

```text
rl:config:<domain>:<sha256(api_key)>  Hash
rl:leases:<actual_domain>:<sha256(selected_key)>  ZSET
rl:lease:<lease_id>  String，值为对应 leases key，带 TTL
```

`selected_key` 在具体 key 规则下是请求 key，在域名规则下是空字符串。配置 Hash 只写 max_concurrent、enabled、description、updated_at，不保存明文 API Key；api_key_hash 由 Redis key 后缀解析或计算后返回，不是单独存储字段。

```text
sha256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

空 key 规则仍有非空摘要，这是正常标识。具体 key 与空 key 的配置是两条独立记录，互不覆盖。

计数从 ZSET 中导出。acquire 使用 Redis TIME 清理过期成员后判定额度；租约指针自身 TTL 到期不代表 ZSET 成员自动消失，后续 acquire 会清理。release 幂等；renew 同时延长指针及成员有效期，不复活已释放、已过期租约。

动态规则切换只影响后续 acquire，已有租约继续在原集合续约和释放，不追溯合并两个计数池。

## Counter Service 与故障边界

| 场景 | 接口结果 | 网关行为 |
| --- | --- | --- |
| 命中启用规则且有额度 | 200 + allowed=true + lease_id | 转发，续约和结束释放 |
| 无启用的匹配规则 | 200 + allowed=true，无 lease_id | 正常转发，不续约、不释放 |
| 启用规则明确超限 | 200 + allowed=false + reason=limit_exceeded | 按 error_response 拒绝，默认429、text/plain |
| Redis 候选 enabled 缺失或不是精确 true/false | 500 + allowed=false + reason=invalid_config | fail-open，不继续回退或计数 |
| 启用规则阈值缺失或不是正整数 | 同上 | 同上 |
| Redis 网络或操作超时 | 503 + allowed=false + reason=redis_unavailable | fail-open |
| 非法 HTTP 请求 | 400/405 | 插件收到非200时 fail-open |

查询遇到损坏配置返回内部500；列表页包含损坏配置时整页报错，不把损坏状态显示为 enabled=false。合法 enabled=false 的候选直接跳过，不评估其阈值。

`config_not_found`、`api_key_disabled` 不再作为拒绝结果。Wasm 收到带这些原因的 allowed=false 时也放行；唯一有效的策略拒绝原因是 limit_exceeded。

Wasm dispatch 失败、非200、超时、空体、非法JSON、缺布尔 allowed、未知原因、超大响应均放行，不回退本地限流。Counter Service acquire 的 Redis 操作共用500ms预算；暂停请求由独立看门狗恢复并忽略迟到的拒绝。续约或释放失败不改变业务响应。

保留 ECDS、Lua透传默认配置、fail_open=true 和 nack_on_code_cache_miss=true。插件初始化失败禁用功能并返回启动成功，下载服务启动不依赖 Redis。先发布 Counter Service，再发布 Wasm 与匹配摘要。

## 验证

```bash
go test -race -count=1 -p=1 ./shared/... ./rate-limiter/... ./token-stats/...
make image VERSION=rules-local DIST_DIR=dist/rules-build
kind load docker-image counter-service:rules-local --name kind
python3 -B scripts/k8s_e2e.py \
  --context kind-kind --image counter-service:rules-local \
  --artifacts dist/rules-build/rules-local/linux-amd64 \
  --rate-limiter-only --output dist/e2e/rules-local
```

Go 命令包含已废弃模块的现有编译和测试，不表示恢复维护。限流专项 E2E 只部署限流插件，通过实际占用槽位验证插件已激活；不依赖缺规则拒绝或 Token 指标。隔离集群验证与生产容量、长期稳定性、实际业务验收分别记录。
