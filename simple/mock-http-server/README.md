# 模拟慢 HTTP 服务

服务部署在 `default` 命名空间，GET、POST 请求默认等待 10 秒后返回 HTTP 200，响应正文为 `OK`。各请求由独立线程处理。Python 脚本通过 ConfigMap 挂载，无需构建业务镜像；使用本地 kind 节点已有的 `python:3.10-slim` 镜像。

## 部署

```bash
kubectl --context kind-kind apply -f simple/mock-http-server/deploy.yaml
kubectl --context kind-kind -n default rollout status deployment/mock-http-server
```

YAML 包含 ConfigMap、Deployment、Service、Istio Gateway、DestinationRule 和 VirtualService，均指定 `namespace: default`。Gateway 通过 `istio: ingressgateway` 选择已有的 `istio-system/istio-ingressgateway`，域名为 `mock-http-server.local`。VirtualService 路由超时为 300 秒。

DestinationRule 的 `current-pod` subset 使用 `app: mock-http-server` 和 `pod-template-hash: 5658c678b`，当前匹配 Pod `mock-http-server-5658c678b-mxw4p`；VirtualService 指定该 subset。该选择器绑定当前 Deployment 版本，同版本扩容或重建的 Pod 也会匹配。滚动更新（包括修改等待时间、镜像或执行 rollout restart）后，须查询新 Pod 的 `pod-template-hash` 并更新 YAML 中的 subset 标签，再 apply，否则旧版本退出后路由将没有可用后端。

```bash
kubectl --context kind-kind -n default get pods -l app=mock-http-server --show-labels
```

## 访问

当前本地 kind 节点 IP 为 `172.18.0.2`，网关 HTTP NodePort 为 `31901`，可从本机访问：

```bash
curl --noproxy '*' --max-time 20 -H 'Host: mock-http-server.local' \
  -w '\nHTTP %{http_code}, elapsed %{time_total}s\n' \
  http://172.18.0.2:31901/
```

也可以转发网关端口，在另一个终端发起请求：

```bash
kubectl --context kind-kind -n istio-system port-forward svc/istio-ingressgateway 18080:80
```

```bash
curl --noproxy '*' --max-time 20 -H 'Host: mock-http-server.local' http://127.0.0.1:18080/
```

## 调整等待时间或脚本

修改 `deploy.yaml` 中 Deployment 的 `--delay` 参数后重新 apply 即可。若增加到 300 秒以上，同时调整 VirtualService 的 `timeout` 和客户端超时。

ConfigMap 内保存了 `script/mock_http_server.py` 的副本。修改脚本后需要同步 ConfigMap 内容，重新 apply，并重启进程：

```bash
kubectl --context kind-kind -n default rollout restart deployment/mock-http-server
kubectl --context kind-kind -n default rollout status deployment/mock-http-server
```

## 卸载

```bash
kubectl --context kind-kind delete -f simple/mock-http-server/deploy.yaml
```

卸载只删除这份 YAML 中的资源，保留现有网关和 `default` 命名空间。
