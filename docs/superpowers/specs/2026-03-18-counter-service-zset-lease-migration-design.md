# Counter-Service ZSET 租约集合迁移设计

> 保持现有基于 `Domain + API Key` 的动态限流配置实现不变，仅将分布式并发占用模型从 `counter + lease` 迁移到 ZSET 租约集合模型，结构性修复 lease 过期导致槽位泄漏的问题。

## 1. 背景

当前 counter-service 已经支持：
- Redis 动态配置：`rl:config:<domain>:<api_key>`
- 配置 fallback：精确域名 → `*.<parent>` → `*`
- WASM 插件异步调用 `/acquire` 与 `/release`

但分布式并发占用模型仍是旧的：
- `rl:counter:<domain>:<api_key>`：独立计数器
- `rl:lease:<lease_id>`：保存 `counter_key` 指针并带 TTL

该模型存在结构性问题：当连接持续时间超过 `lease_ttl_ms` 时，lease key 会自动过期，但 counter 不会同步递减。随后 release 请求会因为 lease key 不存在而失效，造成并发槽位永久泄漏，最终持续返回 429。

本次设计目标是：**保持现有动态配置语义和插件调用契约不变，仅替换底层并发占用数据结构与 Lua 脚本实现。**

## 2. 设计目标

1. 保持现有 `Domain + API Key` 动态配置模型不变
2. 保持现有 wildcard / global fallback 行为不变
3. 保持插件 `/acquire` 与 `/release` 接口契约尽量不变
4. 结构性修复 lease TTL 过期后 counter 泄漏问题
5. 保留 release 的幂等性
6. 在 release 失败时，后续 acquire 仍能通过清理过期 lease 自动恢复

## 3. 方案选择

采用 **方案 A：完全采用 ZSET 租约集合模型**。

不采用后台补偿清理或大 TTL 兜底，因为这些都只是缓解，不是根修。

## 4. 数据结构设计

### 4.1 保持不变的配置结构

继续使用：

```text
rl:config:<domain>:<api_key>     → Hash
```

字段保持不变，例如：
- `max_concurrent`
- `enabled`
- `tier`
- `description`
- `updated_at`

配置查找顺序保持不变：
1. `rl:config:<exact_domain>:<api_key>`
2. `rl:config:*.parent_domain:<api_key>`
3. `rl:config:*:<api_key>`

### 4.2 新的并发占用结构

移除旧结构：

```text
rl:counter:<domain>:<api_key>    → String
```

改为：

```text
rl:leases:<domain>:<api_key>     → ZSET
  - score  = lease 过期时间戳（毫秒）
  - member = lease_id

rl:lease:<lease_id>              → String
  - value = rl:leases:<domain>:<api_key>
  - TTL   = lease_ttl_ms
```

### 4.3 关键语义

- **真实并发数**：`ZCARD rl:leases:<domain>:<api_key>`
- **过期清理**：每次 acquire 前执行 `ZREMRANGEBYSCORE leases_key -inf now_ms`
- **release 凭证**：`rl:lease:<lease_id>` 仍然存在，用于保证 release 幂等
- **lease key 的新含义**：不再指向 counter key，而是指向对应的 `leases_key`

## 5. Acquire / Release 行为设计

## 5.1 Acquire 请求契约

保持现有契约：

```json
POST /acquire
{
  "domain": "api.example.com",
  "api_key": "key_basic_001",
  "ttl_ms": 30000
}
```

## 5.2 Acquire 处理流程

1. 按现有顺序查找配置（精确 → wildcard → global）
2. 校验配置：
   - 配置存在
   - `enabled == true`
   - `max_concurrent > 0`
3. 构造实际并发集合 key：

```text
rl:leases:<request_domain>:<api_key>
```

注意：即使配置命中的是 wildcard / global，**并发集合仍按实际请求 domain + api_key 构造**，不共享跨域并发池。

4. 在 Lua 中原子执行：
   - `ZREMRANGEBYSCORE leases_key -inf now_ms`
   - `ZCARD leases_key`
   - 若 `current >= max_concurrent`，返回 `allowed=false`
   - 否则 `ZADD leases_key expire_at lease_id`
   - `SET rl:lease:<lease_id> leases_key PX ttl_ms`

5. 返回：
- `allowed`
- `lease_id`
- `max_concurrent`
- `current_count`
- `tier`

## 5.3 Release 请求契约

保持对外接口尽量不变：

```json
POST /release
{
  "lease_id": "..."
}
```

WASM 插件当前额外传 `api_key` 也允许保留，但 release 内部逻辑不依赖它。

## 5.4 Release 处理流程

在 Lua 中原子执行：
1. `GET rl:lease:<lease_id>` 获取 `leases_key`
2. 若不存在：返回 `released=false, reason=lease_not_found`
3. `DEL rl:lease:<lease_id>`
4. `ZREM leases_key lease_id`
5. `ZCARD leases_key` 作为 release 后当前活跃数
6. 返回 `released=true, current_count=<zcard_after_release>`

### 幂等性

- 第一次 release：成功删除凭证和 ZSET member
- 第二次 release：lease key 已不存在，返回 `released=false`
- 若 lease key 已因 TTL 过期消失：同样返回 `released=false`
- 即使 ZSET 中残留旧 member，下次 acquire 前的 `ZREMRANGEBYSCORE` 也会清理它

## 6. 为什么这个方案能修复当前问题

当前问题根因是：
- old model 中 counter 与 lease 生命周期分离
- lease 自动过期后 counter 不自动减少

新方案中：
- 不再维护独立 counter
- 活跃并发数直接来源于 ZSET 大小
- 每次 acquire 前主动清理过期 member

因此：
- 即使 release 没有成功执行
- 只要 lease 超过 TTL
- 后续 acquire 也会先清理过期项并恢复可用槽位

这解决的是结构问题，而不是仅仅给 release 增加重试或日志。

## 7. 代码改动边界

### 7.1 必改文件

#### `internal/counter-service/redis/scripts.go`
- 重写 acquire Lua 脚本：从 counter 模型迁移到 ZSET 模型
- 重写 release Lua 脚本：从 `DECR` 改为 `ZREM + ZCARD`
- 保留 list_configs 脚本不变

新 acquire 脚本 KEYS / ARGV 映射：
```
KEYS[1]: config_key        (rl:config:<domain>:<api_key>)
KEYS[2]: leases_key        (rl:leases:<domain>:<api_key>)  ← 原 counter_key
KEYS[3]: lease_key         (rl:lease:<lease_id>)
KEYS[4]: wildcard_config_key
KEYS[5]: global_config_key
ARGV[1]: lease_ttl_ms
ARGV[2]: now_ms            ← 新增
ARGV[3]: lease_id
```

新 release 脚本 KEYS / ARGV 映射：
```
KEYS[1]: lease_key         (rl:lease:<lease_id>)
ARGV[1]: lease_id          ← 新增，用于 ZREM
```

#### `internal/counter-service/redis/operations.go`
- Acquire：
  - `counterKey` 改为 `leasesKey`
  - 新增 `now_ms := time.Now().UnixMilli()`
  - KEYS 顺序：`[configKey, leasesKey, leaseKey, wildcardConfigKey, globalConfigKey]`
  - ARGV 顺序：`[ttl_ms, now_ms, lease_id]`
- Release：
  - 仍只传 `lease_id`
  - KEYS：`[leaseKey]`，ARGV：`[lease_id]`
  - 解析新的返回结构（`released`, `current_count`）

#### 测试文件同步更新
- `operations_test.go` 中断言 `rl:lease:<id>` 的 value 从 `rl:counter:...` 改为 `rl:leases:...`

#### `internal/counter-service/handler/release.go`
- 接口可基本不变
- 保持：lease 不存在时返回 `200 + released=false`

### 7.2 尽量不改文件

#### `internal/plugin/root.go`
- 不改 `/acquire` 调用契约
- 不改 `/release` 调用路径
- 不为本次迁移增加新的插件行为变化

#### 动态配置相关逻辑
- `rl:config:<domain>:<api_key>` 保持不变
- wildcard / global fallback 逻辑保持不变
- CRUD 接口语义保持不变

## 8. 测试设计

## 8.1 Redis / Lua 层测试

必须覆盖：

1. **正常 acquire / release**
   - acquire 成功
   - ZSET 插入 member
   - release 后 member 被删除
   - 再次 acquire 成功

2. **达到上限**
   - `max_concurrent=1`
   - 第一次 acquire 成功
   - 第二次 acquire 返回 `limit_exceeded`

3. **lease 过期后自动恢复**（核心回归测试）
   - acquire 成功，TTL 很短
   - 等待 lease 过期
   - 再次 acquire 时，脚本先清理过期 member
   - 请求应恢复可用，而不是永久卡槽

4. **release 幂等**
   - 第一次 release 成功
   - 第二次 release 返回 `released=false`

5. **wildcard / global fallback 不受影响**
   - 精确配置缺失时命中 wildcard
   - 或命中 global
   - acquire 仍成功
   - 并发集合 key 仍按实际请求 domain 构造

## 8.2 Handler 层测试

### `internal/counter-service/handler/acquire_test.go`
继续验证：
- success
- config_not_found
- api_key_disabled
- invalid_config
- limit_exceeded
- wildcard / global fallback
- lease 过期后再次 acquire 自动恢复

### `internal/counter-service/handler/release_test.go`
继续验证：
- 正常 release 返回 `200 + released=true`
- lease 不存在返回 `200 + released=false`
- Redis 不可用返回 `503`

## 8.3 插件层测试

### `internal/plugin/root_test.go`
本次不改变插件接口契约，但应保留验证：
- acquire success 后保存 lease_id
- `OnHttpStreamDone()` 仍 dispatch release callout

## 9. 验收标准

1. 分布式并发限制在 `domain + api_key` 维度继续生效
2. wildcard / global 配置 fallback 继续生效
3. 请求结束后，正常 release 可立即释放槽位
4. 即使 release 未生效，lease TTL 到期后，后续 acquire 也能自动恢复
5. 不再出现“请求结束后长期持续 429、必须人工清 Redis counter”的情况
6. 插件与 counter-service 的外部调用契约不需要大改

## 10. 非目标

本次不处理：
- 插件 release callback 的可观测性增强
- release 重试机制
- 配置 CRUD API 语义变更
- 跨 domain 共享并发池
- 对 deploy YAML 的结构性调整

这些可以后续单独做，但不应与本次结构性修复耦合在一起。
