# 当前限流契约

本文是 `rate-limiter`、Counter Service 和 Redis 的当前共同契约。部署见 [README](../../README.md)，隔离测试见 [Kubernetes E2E](../k8s-e2e.md)。项目未正式上线，直接使用本契约，不提供旧协议兼容或数据迁移。历史设计文档仅用于追溯。

`token-stats` 已废弃，保留既有代码、构建和部署清单，不属于本次功能或验收范围。

## 服务身份与路由转换

额度身份是 `service + port + API Key`：service仅支持小写的 `命名空间/Service名称`，例如 `ai/inference`；port为必填的Service端口（1～65535整数），不是Pod的targetPort。不同命名空间、服务、端口的额度隔离；多个入口域名和同服务的不同subset共享额度。

Wasm在请求头阶段读取Envoy的 `cluster_name` 属性，原样发送，不解析服务名称，不查询Kubernetes API或DNS。没有域名白名单，也没有本地限流模式。关闭分布式限流时透传；读取失败、空目标或超过1024字节时直接放行。

Counter Service识别 `outbound|port|subset|service.namespace.svc.<集群域>`，转换为namespace/name和port，忽略subset。`KUBERNETES_CLUSTER_DOMAIN`默认 `cluster.local`，可设置其他集群域；配置非法时记录日志并停用服务识别，HTTP服务和Wasm下载仍可用。不支持的cluster或非Kubernetes目标返回 `{"allowed":true}`，不访问Redis、不分配租约。

VirtualService可以填写短名称或FQDN；短名称的namespace由VirtualService自身所在namespace决定，实际匹配使用Istio解析后的目标，与入口Host无关。

## 规则与额度池

按顺序查询：

1. 精确service + port + 具体API Key。
2. 相同service + port + 空API Key的默认规则。

不存在、enabled=false的规则跳过。启用规则一旦命中，额度不足就拒绝，不再回退。全部未匹配时正常放行，不分配租约。

Bearer缺失或格式错误时视为没有可用Key，只查询默认规则。插件不承担认证职责。具体Key与默认规则是独立池，默认规则供未命中具体Key的请求共享，不叠加为服务总并发上限。

## 配置管理接口

PUT `/config`：

```json
{"service":"ai/inference","port":8000,"api_key":"key001","max_concurrent":10,"enabled":true,"description":"推理并发额度"}
```

- service必须为合法的小写namespace/name，不接受裸名称、FQDN或通配符。
- port必填且为1～65535整数；max_concurrent为正整数，enabled必须显式提供布尔值。
- api_key可省略或为空，按原始字节计算SHA256，不去空格、不改大小写。
- api_key_hash可选，出现时必须严格等于SHA256(api_key)的小写十六进制摘要。
- description省略保留原值，非空覆盖，空串删除；字段出现时不接受null。
- 未知字段（包括domain、tier）、未知或重复查询参数返回400；校验失败不写Redis。

GET/DELETE `/config?service=ai/inference&port=8000&api_key=key001` 定位规则。不存在的单项查询返回404，删除幂等。

GET `/configs?cursor=0&limit=100` 只支持cursor/limit。配置对象返回service、port、api_key_hash、max_concurrent、enabled、可选description、updated_at，不返回明文api_key。必须翻页到cursor="0"，空页仍返回configs=[]并保留游标。

编辑具体Key时提供原始Key；查询对象不是写入对象，直接回写应被拒绝。

## 运行时接口

POST `/acquire`：

```json
{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","api_key":"key001","ttl_ms":30000}
```

upstream_cluster为1～1024字节，ttl_ms为正整数。接口不接收service/port覆盖转换结果，没有domain字段。成功占用额度返回allowed=true、lease_id、max_concurrent和current_count；无匹配启用规则或无法识别的目标只返回allowed=true。

POST `/renew` 接收lease_id和正整数ttl_ms，返回renewed。POST `/release` 接收lease_id和可选api_key，返回released。二者均通过租约指针定位原额度池，不重新识别路由。释放幂等，续约不复活已经释放或过期的租约。

## Redis 结构

```text
rl:service:config:<namespace/name>:<port>:<sha256(api_key)>       Hash
rl:service:leases:<namespace/name>:<port>:<sha256(selected_key)> ZSET
rl:lease:<lease_id>                                           String，带TTL，指向leases键
```

REDIS_KEY_PREFIX可替换默认rl:。配置Hash只存max_concurrent、enabled、description、updated_at，不存明文Key或冗余身份字段。默认规则使用sha256("")，具体Key规则使用请求Key摘要。

acquire通过Lua原子选择规则、使用Redis TIME清理过期ZSET成员、判定额度并创建租约。指针TTL到期不代表ZSET成员自动消失，由后续acquire清理。renew同时延长指针和ZSET成员有效期，release移除两者。

规则变更只影响后续acquire；在途租约始终续约和释放原集合，不追溯合并额度。

## 故障边界

| 场景 | 接口结果 | 网关行为 |
| --- | --- | --- |
| 启用规则有额度 | 200，allowed=true，有lease_id | 转发并续约，结束释放 |
| 无启用规则或无法识别目标 | 200，allowed=true，无lease_id | 转发，不续约、不释放 |
| 启用规则明确超限 | 200，allowed=false，reason=limit_exceeded | 按error_response拒绝，默认429 |
| 候选enabled缺失/非法，启用阈值损坏 | 500，allowed=false，reason=invalid_config | 放行，不继续回退或计数 |
| Redis网络故障或操作超时 | 503，allowed=false，reason=redis_unavailable | 放行 |
| 非法请求 | 400/405 | Wasm收到非200时放行 |

合法enabled=false的规则不评估其阈值。查询或列表遇到损坏配置返回500，不把损坏状态显示为禁用。

Wasm调用失败、非200、超时、空体、非法JSON、缺少布尔allowed、未知拒绝原因和超大响应均放行。唯一可拒绝业务的结果是HTTP200 + allowed=false + limit_exceeded，不回退本地限流。

acquire的Redis连接、执行和重试共用500ms预算。Wasm有独立暂停看门狗，恢复后忽略迟到拒绝；续约和释放错误不影响业务响应。业务上游自身错误原样透传。

保留ECDS、Lua透传默认配置、fail_open=true和nack_on_code_cache_miss=true；插件业务配置错误禁用功能并返回启动成功。Counter Service的Wasm下载不依赖Redis健康。

## 验证

```bash
go test -race -count=1 -p=1 ./shared/... ./rate-limiter/... ./token-stats/...
go test ./rate-limiter/internal/counter-service/models -run '^$' -bench ParseUpstreamCluster -benchmem
make artifacts VERSION=service-rules-local
kind load docker-image counter-service:service-rules-local --name kind
python3 -B scripts/k8s_e2e.py --context kind-kind --rate-limiter-only \
  --image counter-service:service-rules-local \
  --artifacts dist/service-rules-local/linux-amd64 --output dist/e2e/service-rules-local
```

Go测试包含已废弃模块的既有回归，不表示恢复维护。真实E2E通过占满租约后的429确认插件激活，并核对实际cluster、acquire载荷与服务端口额度池；不能仅凭业务200判断限流有效。本地功能、制品检查与生产容量/稳定性验收分别记录。
