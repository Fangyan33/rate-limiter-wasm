# rate-limiter-wasm E2E 测试要求

本文件按当前实现更新，原2026-03-24草稿中的“缺规则/禁用即拒绝”和统计验收要求已替代。行为规范见[当前限流契约](../specs/rate-limiting-contract.md)，执行方法见[Kubernetes E2E](../k8s-e2e.md)。

## 验证目标

验证 Counter Service + Redis、双副本 Istio 网关 + 真实 Wasm + 模拟业务上游。仅命中插件 domains 的请求进入限流，域名之外直接旁路。

具体 key 按精确域名、直接父域、全局顺序查找，再查空 key 的相同三级域名规则；禁用逐条跳过，无规则直接放行。启用规则超限才按 error_response 拒绝，默认429、text/plain；配置损坏或基础设施故障 fail-open，不回退本地限流。

## 必测行为

1. CRUD 与分页：配置键使用SHA256，查询JSON无api_key/tier；分页直到cursor=0，空页返回数组。
2. 写入校验：enabled必填布尔；hash如出现必须等于SHA256(api_key)；空值/null/错误类型或不匹配返回400，Redis原记录和时间戳不变；tier写入400。
3. 域名统一小写；description省略保留、非空覆盖、空串删除。
4. 六级匹配顺序、跳过禁用规则、未知/缺失/非法凭证回退域名，无候选时200放行且不产生租约。
5. 具体key超限后不借用域名额度；具体key和域名池分别计数，同key跨域独立计数。
6. 并发、释放幂等、TTL过期清理、续约、长流和跨副本全局一致性。规则切换不合并原有租约池。
7. 直接破坏Redis enabled及启用阈值时内部500、网关业务继续，不能把损坏状态显示或解释成明确禁用。
8. Counter Service dispatch、非200、超时、空体、非法/超大响应、缺allowed、旧禁用/缺配置原因等故障均放行。renew/release故障不影响业务响应。
9. Redis实际停机后直接acquire在800ms内得到完整内部503，网关收到该503后继续上游请求；恢复后正常限流。
10. ECDS默认透传、下载故障、无效配置/模块、初始化失败、运行时trap、无下载源冷启动；期间两个副本业务不中断。

## 环境与数据

- 使用本地Kubernetes集群（当前context为 `kind-kind`），复用已有Istio控制面，创建独立命名空间、两个测试网关副本、临时Redis、Counter Service及HTTP/SSE fixture。
- 先导入测试镜像，使用 `scripts/k8s_e2e.py --rate-limiter-only`。该模式不部署token-stats，也不要求extraStatTags。
- 业务路由由脚本生成；人工演示资源在simple/nginx和simple/vllm-mock，不使用不存在的deploy/nginx或simple/istio.yaml路径。
- 域名使用api.e2e.test及其余测试子域，未命中域使用unmatched.test。API Key只要求有效Bearer字符串，不要求JWT或uid。
- 必须先创建实际规则并占满额度，验证两个副本均超限拒绝后清理探针，不能把透传默认配置当作Wasm已经激活。

## 验收与证据

全部本轮限流断言通过，报告包含实际请求/响应、配置、日志、故障注入及资源清理结果。结束后原有Deployment/EnvoyFilter/ConfigMap配置与副本数保持一致。

单元/race、静态清单、隔离集群功能验收分别记录；大规模容量、长时间稳定性和真实模型上游验收另行安排，不从旧报告推导当前通过。

## 已废弃统计功能

token-stats停止维护，保留现有代码、构建、部署清单和旧完整测试入口，后续完整移除。UID精度、指标基数、usage注入或旧指标提取方案不再作为限流改动的设计、修复或验收要求。
