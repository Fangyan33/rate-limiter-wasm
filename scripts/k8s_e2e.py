#!/usr/bin/env python3
"""在本地 Kubernetes/Istio 中运行隔离的真实 WASM E2E；只使用 Python 标准库。"""

import argparse
import base64
import concurrent.futures
import hashlib
import http.client
import json
import socket
import subprocess
import sys
import threading
import time
import traceback
from pathlib import Path
from urllib.parse import urlencode


ROOT = Path(__file__).resolve().parents[1]


def check(condition, detail):
    if not condition:
        raise AssertionError(detail)


def eventually(fn, timeout=12):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = fn()
            if last:
                return last
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.1)
    raise AssertionError(f"等待条件超时 ({timeout}s): {last!r}")


def request(port, path, method="GET", data=None, headers=None):
    hdr = dict(headers or {})
    if data is not None and not isinstance(data, bytes):
        data = json.dumps(data).encode()
        hdr.setdefault("Content-Type", "application/json")
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    try:
        conn.request(method, path, data, hdr)
        response = conn.getresponse()
        body = response.read().decode()
        try:
            parsed = json.loads(body)
        except ValueError:
            parsed = body
        return {"status": response.status, "headers": dict(response.getheaders()),
                "body": parsed, "raw": body}
    finally:
        conn.close()


def jwt(uid):
    def enc(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    return enc({"alg": "HS256"}) + "." + enc({"uid": uid}) + ".e2esignature"


class Suite:
    def __init__(self, args):
        self.args = args
        self.ns = args.namespace
        self.out = Path(args.output).resolve()
        self.out.mkdir(parents=True, exist_ok=True)
        self.forwards = []
        self.created = False
        self.cleaned = False
        self.results = []
        self.key = jwt("basic_001")
        self.key2 = jwt("premium_001")
        self.host = "api.e2e.test"
        self.service = self.ns + "/fixture"
        self.peer_ns = self.ns + "-peer"
        self.pool = concurrent.futures.ThreadPoolExecutor(max_workers=8)

    def k(self, *args, data=None, timeout=60):
        result = subprocess.run(["kubectl", "--context", self.args.context, *args],
                                input=json.dumps(data) if data is not None else None,
                                text=True, capture_output=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError(f"kubectl {' '.join(args)}: {result.stderr or result.stdout}")
        return result.stdout

    def save(self, name, value):
        (self.out / name).write_text(value if isinstance(value, str) else
                                     json.dumps(value, indent=2, ensure_ascii=False))

    def run_case(self, name, fn):
        started = time.monotonic()
        try:
            detail = fn()
            result = {"name": name, "status": "PASS", "detail": detail}
        except Exception as exc:
            result = {"name": name, "status": "FAIL", "detail": str(exc),
                      "traceback": traceback.format_exc()}
        result["seconds"] = round(time.monotonic() - started, 3)
        self.results.append(result)
        print(f"{result['status']} {name}: {result['detail']}", flush=True)
        self.save("results.json", self.results)

    def manifest(self):
        ns = self.ns
        docs = []

        def obj(kind, name, spec=None, **extra):
            api = ("apps/v1" if kind == "Deployment" else
                   "networking.istio.io/v1alpha3" if kind in
                   ("EnvoyFilter", "Gateway", "VirtualService", "DestinationRule") else "v1")
            result = {"apiVersion": api, "kind": kind,
                      "metadata": {"name": name, "namespace": ns}, **extra}
            if spec is not None:
                result["spec"] = spec
            docs.append(result)
            return result

        def service(name, port=8080):
            obj("Service", name, {"selector": {"app": name},
                                  "ports": [{"name": "http" if port == 8080 else "tcp-redis",
                                             "port": port, "targetPort": port}]})

        def deployment(name, container, **pod_extra):
            return obj("Deployment", name, {"replicas": 1,
                       "selector": {"matchLabels": {"app": name}},
                       "template": {"metadata": {"labels": {"app": name},
                                    "annotations": {"sidecar.istio.io/inject": "false"}},
                                    "spec": {"containers": [container], **pod_extra}}})

        deployment("redis", {"name": "redis", "image": "redis:8.6.1",
                    "imagePullPolicy": "Never", "args": ["--save", "", "--appendonly", "no"],
                    "readinessProbe": {"exec": {"command": ["redis-cli", "ping"]},
                                       "periodSeconds": 2}})
        service("redis", 6379)
        deployment("counter-service", {"name": "counter-service", "image": self.args.image,
                    "imagePullPolicy": "Never", "env": [{"name": "REDIS_ADDR", "value": "redis:6379"}],
                    "readinessProbe": {"httpGet": {"path": "/health", "port": 8080},
                                       "periodSeconds": 2}})
        service("counter-service")
        fixture_config = obj("ConfigMap", "fixture", data={"server.py": (ROOT / "scripts/e2e_fixture.py").read_text()})
        if self.args.fault_wasm:
            fixture_config["binaryData"] = {"fault.wasm": base64.b64encode(Path(self.args.fault_wasm).read_bytes()).decode()}
        deployment("fixture", {"name": "fixture", "image": "python:3.10-slim",
                    "imagePullPolicy": "Never", "command": ["python", "/app/server.py"],
                    "env": [{"name": "COUNTER_ADDR", "value": "counter-service:8080"}],
                    "volumeMounts": [{"name": "code", "mountPath": "/app"}],
                    "readinessProbe": {"httpGet": {"path": "/health", "port": 8080},
                                       "periodSeconds": 2}},
                    volumes=[{"name": "code", "configMap": {"name": "fixture"}}])
        obj("Service", "fixture", {"selector": {"app": "fixture"}, "ports": [
            {"name": "http-main", "port": 8080, "targetPort": 8080},
            {"name": "http-alternate", "port": 8081, "targetPort": 8080}]})
        obj("Service", "fixture-alt", {"selector": {"app": "fixture"}, "ports": [
            {"name": "http", "port": 8080, "targetPort": 8080}]})
        obj("DestinationRule", "fixture", {"host": f"fixture.{ns}.svc.cluster.local", "subsets": [
            {"name": "v1", "labels": {"app": "fixture"}},
            {"name": "v2", "labels": {"app": "fixture"}}]})
        gateway = deployment("e2e-gateway", {"name": "istio-proxy", "image": "auto",
                    "imagePullPolicy": "IfNotPresent", "env": [
                        {"name": "ISTIO_META_UNPRIVILEGED_POD", "value": "true"}],
                    "ports": [{"containerPort": 8080}],
                    "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}})
        gateway["spec"]["replicas"] = 2
        meta = gateway["spec"]["template"]["metadata"]
        meta["labels"]["istio"] = "rate-limiter-e2e"
        meta["annotations"] = {"sidecar.istio.io/inject": "true",
                               "inject.istio.io/templates": "gateway",
                               "proxy.istio.io/config": '{"concurrency":2}'}
        obj("Gateway", "e2e", {"selector": {"app": "e2e-gateway", "istio": "rate-limiter-e2e"},
            "servers": [{"port": {"number": 8080, "name": "http-e2e", "protocol": "HTTP"},
                         "hosts": ["*.e2e.test", "unmatched.test", "no-route.test"]}]})
        def route(prefix, host, port=8080, subset=None):
            destination = {"host": host, "port": {"number": port}}
            if subset:
                destination["subset"] = subset
            return {"match": [{"uri": {"prefix": prefix}}], "route": [{"destination": destination}], "timeout": "20s"}
        routes = [route("/short", "fixture"), route("/port", "fixture", 8081),
                  route("/other", "fixture-alt"), route("/peer", f"fixture.{self.peer_ns}.svc.cluster.local")]
        routes.append({"match": [{"uri": {"prefix": "/subset"}}], "route": [
            {"destination": {"host": "fixture", "subset": subset, "port": {"number": 8080}}, "weight": 50}
            for subset in ("v1", "v2")], "timeout": "20s"})
        routes.append({"match": [{"uri": {"prefix": "/weighted"}}], "route": [
            {"destination": {"host": host, "port": {"number": 8080}}, "weight": 50}
            for host in ("fixture", "fixture-alt")], "timeout": "20s"})
        routes.append(route("/", f"fixture.{ns}.svc.cluster.local"))
        obj("VirtualService", "e2e", {"hosts": ["*.e2e.test", "unmatched.test"], "gateways": ["e2e"], "http": routes})
        for plugin in (("rate-limiter",) if self.args.rate_limiter_only else ("rate-limiter", "token-stats")):
            digest = hashlib.sha256((Path(self.args.artifacts) / (plugin + ".wasm")).read_bytes()).hexdigest()
            config = "" if plugin == "rate-limiter" else "domains:\n  - '*.e2e.test'\n"
            if plugin == "rate-limiter":
                config += ("distributed_limit:\n  enabled: true\n  backend: counter_service\n"
                           "  counter_service:\n    cluster: e2e-counter\n    acquire_path: /acquire\n"
                           "    release_path: /release\n    renew_path: /renew\n"
                           "    timeout_ms: 800\n    lease_ttl_ms: 1500\n"
                           "error_response:\n  status_code: 429\n  message: Rate limit exceeded\n")
            else:
                config += ("token_statistics:\n  enabled: true\n  inject_stream_usage: true\n"
                           "  metric_key_limit: 8\n")
            patches = []
            if plugin == "rate-limiter":
                for name, target in (("e2e-counter", "fixture"), ("e2e-wasm", "counter-service")):
                    patches.append({"applyTo": "CLUSTER", "match": {"context": "GATEWAY"},
                        "patch": {"operation": "ADD", "value": {"name": name, "type": "STRICT_DNS",
                            "connect_timeout": "1s", "load_assignment": {"cluster_name": name,
                                "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {"socket_address": {
                                    "address": f"{target}.{ns}.svc.cluster.local", "port_value": 8080}}}}]}]}}}})
            patches.append({"applyTo": "HTTP_FILTER", "match": {"context": "GATEWAY",
                "listener": {"filterChain": {"filter": {"name": "envoy.filters.network.http_connection_manager",
                    "subFilter": {"name": "envoy.filters.http.router"}}}}},
                "patch": {"operation": "INSERT_BEFORE", "value": {"name": "envoy.filters.http.wasm",
                    "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm",
                        "config": {"name": plugin, "root_id": plugin + "-root", "vm_config": {
                            "vm_id": plugin + "-vm", "runtime": "envoy.wasm.runtime.v8", "code": {"remote": {
                                "http_uri": {"uri": f"http://counter-service.{ns}.svc.cluster.local:8080/wasm/{plugin}.wasm",
                                             "cluster": "e2e-wasm", "timeout": "10s"}, "sha256": digest}}},
                            "configuration": {"@type": "type.googleapis.com/google.protobuf.StringValue",
                                              "value": config}}}}}})
            http_filter = patches.pop()
            value = http_filter["patch"]["value"]
            wasm_config = value.pop("typed_config")
            wasm_config["config"]["fail_open"] = True
            wasm_config["config"]["vm_config"]["nack_on_code_cache_miss"] = True
            name = plugin + "-wasm"
            value["name"] = name
            value["config_discovery"] = {
                "config_source": {"ads": {}}, "apply_default_config_without_warming": True,
                "default_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua",
                                   "inline_code": "function envoy_on_request(handle) end"},
                "type_urls": ["type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm",
                              "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua"]}
            patches.append({"applyTo": "EXTENSION_CONFIG", "match": {"context": "GATEWAY"},
                            "patch": {"operation": "ADD", "value": {"name": name,
                                                                      "typed_config": wasm_config}}})
            patches.append(http_filter)
            obj("EnvoyFilter", plugin, {"workloadSelector": {"labels": {
                "app": "e2e-gateway", "istio": "rate-limiter-e2e"}}, "configPatches": patches})
        return {"apiVersion": "v1", "kind": "List", "items": docs}

    def forward(self, target, *remote):
        ports = []
        for _ in remote:
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                ports.append(sock.getsockname()[1])
        log = open(self.out / (target.replace("/", "-") + "-forward.log"), "w")
        process = subprocess.Popen(["kubectl", "--context", self.args.context, "-n", self.ns,
            "port-forward", target, *[f"{local}:{dest}" for local, dest in zip(ports, remote)],
            "--address", "127.0.0.1"], stdout=log, stderr=subprocess.STDOUT)
        self.forwards.append((process, log))

        def ready():
            check(process.poll() is None, f"port-forward 已退出: {target}")
            with socket.create_connection(("127.0.0.1", ports[0]), timeout=0.5):
                return True

        eventually(ready, 20)
        return ports

    def setup(self):
        self.api_server = self.k("config", "view", "--minify", "-o",
                                 "jsonpath={.clusters[0].cluster.server}").strip()
        print(f"E2E 本地集群：{self.args.context} ({self.api_server})", flush=True)
        self.baseline = self.inventory()
        self.save("cluster-before.json", self.baseline)
        self.k("create", "-f", "-", data={"apiVersion": "v1", "kind": "Namespace",
            "metadata": {"name": self.ns, "labels": {"purpose": "rate-limiter-e2e", "istio-injection": "enabled"}}})
        self.created = True
        manifest = self.manifest()
        self.save("manifest.json", manifest)
        # Envoy 1.21 下载 remote WASM 失败后不会因业务端点就绪而自动重试。
        # 先确保下载源可用，再创建带 WASM 过滤器的网关监听器。
        base = [item for item in manifest["items"] if item["kind"] in ("Service", "ConfigMap") or
                item["kind"] == "Deployment" and item["metadata"]["name"] != "e2e-gateway"]
        self.k("apply", "--server-side", "-f", "-", data={"apiVersion": "v1", "kind": "List", "items": base})
        for name in ("redis", "counter-service", "fixture"):
            self.k("-n", self.ns, "rollout", "status", "deployment/" + name, "--timeout=180s", timeout=190)
        self.k("apply", "--server-side", "-f", "-", data=manifest)
        self.k("-n", self.ns, "rollout", "status", "deployment/e2e-gateway", "--timeout=180s", timeout=190)
        self.cs = self.forward("service/counter-service", 8080)[0]
        self.fixture = self.forward("service/fixture", 8080)[0]
        pods = json.loads(self.k("-n", self.ns, "get", "pods", "-l", "app=e2e-gateway", "-o", "json"))["items"]
        self.pods = sorted(p["metadata"]["name"] for p in pods)
        check(len(self.pods) == 2, "需要两个就绪的网关副本")
        self.gw = [self.forward("pod/" + p, 8080, 15090, 15000) for p in self.pods]
        self.save("environment.json", {"context": self.args.context, "api_server": self.api_server,
            "namespace": self.ns,
            "image": self.args.image, "pods": self.pods, "ttl_ms": 1500,
            "rate_limiter_only": self.args.rate_limiter_only, "git": subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()})
        eventually(lambda: self.gateway(host="unmatched.test")["status"] == 200, 40)
        # ECDS starts with a pass-through default. Confirm rate limiting is active
        # before testing policy behavior, rather than mistaking fallback for success.
        fixture = json.loads(self.k("-n", self.ns, "get", "pods", "-l", "app=fixture", "-o", "json"))["items"][0]
        peer = {"apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": self.peer_ns}},
            {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "fixture", "namespace": self.peer_ns},
             "spec": {"ports": [{"name": "http", "port": 8080, "targetPort": 8080}]}},
            {"apiVersion": "v1", "kind": "Endpoints", "metadata": {"name": "fixture", "namespace": self.peer_ns},
             "subsets": [{"addresses": [{"ip": fixture["status"]["podIP"]}], "ports": [{"name": "http", "port": 8080}]}]}]}
        self.k("apply", "-f", "-", data=peer)
        self.assert_limiter_active()

    def assert_limiter_active(self):
        # A real occupied lease distinguishes an active limiter from the ECDS Lua default.
        self.config(self.service, self.key, 1)
        lease = None
        try:
            acquired = self.acquire(self.service, self.key, ttl=120000)
            check(acquired["status"] == 200 and acquired["body"].get("lease_id"), acquired)
            lease = acquired["body"]["lease_id"]
            eventually(lambda: all(self.gateway(pod=i, key=self.key)["status"] == 429 for i in range(2)), 60)
        finally:
            if lease:
                self.release(lease)
            self.delete(self.service, self.key)

    def inventory(self):
        data = json.loads(self.k("get", "deployments,envoyfilters,configmaps", "-A", "-o", "json"))
        return [{"kind": item["kind"], "namespace": item["metadata"]["namespace"],
                 "name": item["metadata"]["name"], "spec": item.get("spec"),
                 "data": item.get("data")} for item in data["items"]
                if item["metadata"]["namespace"] != self.ns]

    def api(self, path, data=None, method=None):
        return request(self.cs, path, method or ("POST" if data is not None else "GET"), data)

    def config(self, service, key="", limit=1, enabled=True, port=8080):
        response = self.api("/config", {"service": service, "port": port, "api_key": key,
                            "max_concurrent": limit, "enabled": enabled}, "PUT")
        check(response["status"] == 200, response)

    def delete(self, service, key="", port=8080):
        return self.api("/config?" + urlencode({"service": service, "port": port, "api_key": key}), method="DELETE")

    def cluster(self, service=None, port=8080, subset=""):
        namespace, name = (service or self.service).split("/")
        return f"outbound|{port}|{subset}|{name}.{namespace}.svc.cluster.local"

    def acquire(self, service, key="", ttl=1500, port=8080, subset=""):
        return self.api("/acquire", {"upstream_cluster": self.cluster(service, port, subset), "api_key": key, "ttl_ms": ttl})

    def release(self, lease):
        return self.api("/release", {"lease_id": lease})

    def gateway(self, pod=0, host=None, key=None, data=None, authorization=None, path="/v1/chat/completions"):
        headers = {"Host": host or self.host}
        if authorization is not None:
            headers["Authorization"] = authorization
        elif key is not None:
            headers["Authorization"] = "Bearer " + key
        return request(self.gw[pod][0], path, "POST",
                       data if data is not None else {"messages": [{"role": "user", "content": "hello"}]}, headers)

    def control(self, **state):
        check(request(self.fixture, "/control", "PUT", state)["status"] == 200, state)

    def events(self, endpoint, **fields):
        events = request(self.fixture, "/control")["body"]["events"]
        return [e for e in events if e["path"] == "/" + endpoint and
                all(e["request"].get(k) == v for k, v in fields.items())]

    def redis(self, *args):
        return self.k("-n", self.ns, "exec", "deployment/redis", "--", "redis-cli", "--raw", *args).strip()

    def leases(self, service=None, key=None, port=8080):
        service = service or self.service
        key = self.key if key is None else key
        name = f"rl:service:leases:{service}:{port}:" + hashlib.sha256(key.encode()).hexdigest()
        return int(self.redis("ZCARD", name))

    def idle(self, service=None, key=None, port=8080):
        eventually(lambda: self.leases(service, key, port) == 0)

    def metrics(self):
        return "\n".join(request(p[1], "/stats/prometheus")["raw"] for p in self.gw)

    def metric(self, name, uid, domain=None):
        total = 0
        for line in self.metrics().splitlines():
            if line.startswith(name + "{") and f'uid="{uid}"' in line and f'domain="{domain or self.host}"' in line:
                total += float(line.rsplit(" ", 1)[1])
        return total

    def counter_cases(self):
        def crud():
            service = self.ns + "/crud"
            self.config(service, "opaque", 2)
            query = "/config?" + urlencode({"service": service, "port": 8080, "api_key": "opaque"})
            item = self.api(query)["body"]
            check(item["service"] == service and item["port"] == 8080 and "domain" not in item and "api_key" not in item, item)
            check(item["api_key_hash"] == hashlib.sha256(b"opaque").hexdigest(), item)
            self.config(service, "opaque", 3)
            check(self.api(query)["body"]["max_concurrent"] == 3, "更新失败")
            self.delete(service, "opaque")
            check(self.api(query)["status"] == 404, "删除失败")
            return "service/port CRUD、SHA256 和无 domain 响应契约正确"
        self.run_case("CS-服务配置CRUD", crud)

        def import_cli():
            service = self.ns + "/imported"
            key = "import-test-key"
            rule = {"service": service, "port": 8080, "api_key": key, "max_concurrent": 2, "enabled": True}
            source = self.out / "import-rules.json"
            source.write_text(json.dumps([rule]))
            command = ["bash", str(ROOT / "scripts/import-service-configs.sh"), "-f", str(source), "-u", f"http://127.0.0.1:{self.cs}"]
            result = subprocess.run(command, text=True, capture_output=True, timeout=20)
            check(result.returncode == 0 and key not in result.stdout + result.stderr, result.stdout + result.stderr)
            query = "/config?" + urlencode({"service": service, "port": 8080, "api_key": key})
            check(self.api(query)["body"]["max_concurrent"] == 2, "导入未生效")
            source.write_text(json.dumps([{**rule, "enabled": "bad"}]))
            result = subprocess.run(command, text=True, capture_output=True, timeout=20)
            check(result.returncode != 0 and key not in result.stdout + result.stderr, result.stdout + result.stderr)
            check(self.api(query)["body"]["max_concurrent"] == 2, "错误导入修改了已有规则")
            self.delete(service, key)
            return "JSON规则导入成功，非法规则失败退出且不修改原值，输出无明文Key"
        self.run_case("CS-服务规则导入脚本", import_cli)

        def pagination():
            check(self.api("/configs?cursor=0&limit=2")["body"]["configs"] == [], "空页不是空数组")
            for n in range(15):
                self.config(self.ns + f"/page{n}", "page")
            cursor, seen = "0", set()
            for _ in range(100):
                response = self.api("/configs?" + urlencode({"cursor": cursor, "limit": 2}))
                check(response["status"] == 200, response)
                for item in response["body"]["configs"]:
                    check("api_key" not in item and "domain" not in item, item)
                    seen.add((item["service"], item["port"]))
                cursor = response["body"]["cursor"]
                if cursor == "0":
                    break
            check(all((self.ns + f"/page{n}", 8080) in seen for n in range(15)), seen)
            return "完整翻页收齐15条服务规则，空页与摘要字段正确"
        self.run_case("CS-分页列表", pagination)

        def validation():
            payload = {"service": self.service, "port": 8080, "enabled": True, "max_concurrent": 1}
            invalid = [{**payload, "service": v} for v in ("fixture", f"fixture.{self.ns}.svc.cluster.local", "*", self.ns + "/*")]
            invalid += [{**payload, "port": v} for v in (None, 0, 65536, 1.5, "8080")]
            invalid += [{**payload, "domain": self.host}, {**payload, "enabled": None}]
            for data in invalid:
                check(self.api("/config", data, "PUT")["status"] == 400, data)
            for path in ("/config?domain=example.com", "/configs?domain=example.com", "/config?service=" + self.service):
                check(self.api(path)["status"] == 400, path)
            for data in ({"domain": self.host, "ttl_ms": 1000}, {"upstream_cluster": self.cluster(), "service": self.service, "ttl_ms": 1000}):
                check(self.api("/acquire", data)["status"] == 400, data)
            for path in ("/acquire", "/renew", "/release"):
                check(self.api(path + "?domain=x", {}, "POST")["status"] == 400, path)
            return "仅namespace/name有效；端口必填；domain和身份覆盖字段拒绝"
        self.run_case("CS-新契约输入校验", validation)

        def policies():
            service = self.ns + "/policy"
            self.config(service, "", 1)
            self.config(service, "known", 1)
            key_lease = self.acquire(service, "known")["body"]
            check(self.acquire(service, "known")["body"].get("reason") == "limit_exceeded", "满额规则错误回退")
            default_lease = self.acquire(service, "unknown")["body"]
            check(default_lease.get("lease_id"), default_lease)
            self.config(service, "known", 1, enabled=False)
            check(self.acquire(service, "known")["body"].get("reason") == "limit_exceeded", "禁用key未匹配默认额度")
            self.release(key_lease["lease_id"])
            check(self.acquire(service, "known")["body"].get("reason") == "limit_exceeded", "释放key影响了默认池")
            self.release(default_lease["lease_id"])
            self.config(service, "", 1, enabled=False)
            check(self.acquire(service, "known")["body"] == {"allowed": True}, "全部禁用未放行")
            check(self.acquire(self.ns + "/missing")["body"] == {"allowed": True}, "无规则产生租约")
            check(self.api("/release", {"lease_id": default_lease["lease_id"]})["body"]["released"] is False, "重复释放不幂等")
            return "key优先、满额不回退、禁用回退、独立额度池和无规则放行通过"
        self.run_case("CS-规则优先级与独立额度池", policies)

        def ttl():
            service = self.ns + "/ttl"
            self.config(service, "ttl")
            first = self.acquire(service, "ttl", 600)["body"]
            time.sleep(0.3)
            check(self.api("/renew", {"lease_id": first["lease_id"], "ttl_ms": 1600})["body"]["renewed"], "续约失败")
            time.sleep(0.5)
            check(self.acquire(service, "ttl")["body"].get("reason") == "limit_exceeded", "续约无效")
            self.release(first["lease_id"])
            check(not self.api("/renew", {"lease_id": first["lease_id"], "ttl_ms": 1600})["body"]["renewed"], "租约复活")
            self.acquire(service, "ttl", 100)
            time.sleep(0.2)
            next_lease = self.acquire(service, "ttl")["body"]
            check(next_lease["current_count"] == 1, next_lease)
            self.release(next_lease["lease_id"])
            return "续约保持额度、释放不复活、TTL到期回收通过"
        self.run_case("CS-TTL续约释放", ttl)

        def unsupported():
            for cluster in ("PassthroughCluster", "BlackHoleCluster", "outbound|8080||external.example.com", "outbound|8080||fixture.ai.svc.other"):
                result = self.api("/acquire", {"upstream_cluster": cluster, "ttl_ms": 1000})
                check(result["status"] == 200 and result["body"] == {"allowed": True}, result)
            return "非Kubernetes目标不创建租约并正常放行"
        self.run_case("CS-无法识别目标放行", unsupported)

    def gateway_cases(self):
        self.config(self.service, self.key)
        self.config(self.service, self.key2, 2)

        def route_identity():
            self.control(clear=True)
            for host, path in ((self.host, "/v1/chat/completions"), ("second.e2e.test", "/short")):
                response = self.gateway(host=host, key=self.key, path=path)
                check(response["status"] == 200, response)
                self.idle()
            events = self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key)
            check(len(events) == 2, events)
            check(all("domain" not in e["request"] and "service" not in e["request"] for e in events), events)
            check(all(json.loads(e["body"]).get("lease_id") for e in events), events)
            self.save("route-identity.json", events)
            return "短名和FQDN路由均取得真实cluster；Wasm发送原值，CounterService成功创建租约"
        self.run_case("GW-真实路由目标与短名FQDN", route_identity)

        def shared_domains():
            self.idle(); self.control(clear=True)
            held = self.pool.submit(self.gateway, 0, self.host, self.key, {"stream": True, "hold_seconds": 5})
            event = eventually(lambda: self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key))[-1]
            lease = json.loads(event["body"])["lease_id"]
            time.sleep(2.3)
            renewals = self.events("renew", lease_id=lease)
            check(len(renewals) >= 2 and int(self.redis("PTTL", "rl:lease:" + lease)) > 0, renewals)
            check(self.gateway(pod=1, host="second.e2e.test", key=self.key)["status"] == 429, "不同域名未共享额度")
            check(held.result()["status"] == 200, "原SSE异常")
            self.idle()
            check(self.gateway(pod=1, host="second.e2e.test", key=self.key)["status"] == 200, "释放后未恢复")
            self.idle(); self.save("long-stream-renewals.json", request(self.fixture, "/control")["body"])
            return "跨域名、跨双副本共享额度，超过初始TTL继续续约，释放后恢复"
        self.run_case("GW-跨域名共享及长请求续约", shared_domains)

        def default_pool():
            self.config(self.service, "", 1)
            try:
                held = self.pool.submit(self.gateway, 0, None, None, {"stream": True, "hold_seconds": 2})
                eventually(lambda: self.leases(key="") == 1)
                for auth in ("Basic malformed", "Bearer unknown"):
                    check(self.gateway(pod=1, authorization=auth)["status"] == 429, auth)
                check(self.gateway(pod=1, key=self.key)["status"] == 200, "具体key不应占用默认池")
                check(held.result()["status"] == 200, "无头请求异常")
                self.idle(key="")
            finally:
                self.delete(self.service)
            return "缺少凭据、非法头和未知key共用默认池，具体key独立"
        self.run_case("GW-服务端口默认额度", default_pool)

        def isolation():
            identities = [(self.service, 8081, "/port"), (self.ns + "/fixture-alt", 8080, "/other"), (self.peer_ns + "/fixture", 8080, "/peer")]
            for service, port, path in identities:
                self.config(service, self.key, port=port)
            try:
                held = self.pool.submit(self.gateway, 0, None, self.key, {"stream": True, "hold_seconds": 3})
                eventually(lambda: self.leases() == 1)
                for service, port, path in identities:
                    response = self.gateway(pod=1, key=self.key, path=path)
                    check(response["status"] == 200, response)
                    events = self.events("acquire", upstream_cluster=self.cluster(service, port), api_key=self.key)
                    check(events and json.loads(events[-1]["body"]).get("lease_id"), events)
                    self.idle(service, self.key, port)
                check(self.gateway(pod=1, key=self.key)["status"] == 429, "主服务未保持占用")
                check(held.result()["status"] == 200, "主流失败");self.idle()
            finally:
                for service, port, path in identities:
                    self.delete(service, self.key, port)
            return "不同Service、namespace和Service端口额度隔离；8081→targetPort8080按8081计数"
        self.run_case("GW-服务命名空间端口隔离", isolation)

        def weighted():
            self.control(clear=True)
            self.config(self.service, self.key, 1)
            self.config(self.ns + "/fixture-alt", self.key, 1)
            try:
                for path in ("/subset", "/weighted"):
                    for _ in range(30):
                        check(self.gateway(key=self.key, path=path)["status"] == 200, "加权路由失败")
                        self.idle();self.idle(self.ns + "/fixture-alt")
                events = self.events("acquire", api_key=self.key)
                seen = {e["request"]["upstream_cluster"] for e in events}
                expected = {self.cluster(subset="v1"), self.cluster(subset="v2"), self.cluster(), self.cluster(self.ns + "/fixture-alt")}
                check(expected <= seen, seen)
                for event in events:
                    result = json.loads(event["body"])
                    check(result.get("lease_id") and result.get("max_concurrent") == 1, event)
                # Occupy the common service pool, then both weighted subsets must deny.
                occupied = self.acquire(self.service, self.key, ttl=10000)["body"]["lease_id"]
                try:
                    for _ in range(8):
                        check(self.gateway(key=self.key, path="/subset")["status"] == 429, "subset意外拆分额度")
                finally:
                    self.release(occupied)
                self.save("weighted-route-targets.json", events)
                return "读取加权路由实际选中cluster；v1/v2共享服务端口池，跨服务分别计数"
            finally:
                self.delete(self.ns + "/fixture-alt", self.key)
        self.run_case("GW-subset及加权路由", weighted)

        def hot_update():
            held = self.pool.submit(self.gateway, 0, None, self.key, {"stream": True, "hold_seconds": 2.5})
            eventually(lambda: self.leases() == 1)
            check(self.gateway(pod=1, key=self.key)["status"] == 429, "更新前未拒绝")
            self.config(self.service, self.key, 2)
            check(self.gateway(pod=1, key=self.key)["status"] == 200, "更新未生效")
            check(held.result()["status"] == 200, "原流异常");self.idle()
            self.delete(self.service, self.key)
            check(self.gateway(key=self.key)["status"] == 200, "删除规则未放行")
            self.config(self.service, self.key)
            return "阈值即时更新，删除规则放行"
        self.run_case("GW-服务规则热更新", hot_update)

        def corrupt():
            key = "corrupt"
            redis_key = f"rl:service:config:{self.service}:8080:" + hashlib.sha256(key.encode()).hexdigest()
            self.config(self.service, "", 100)
            try:
                for field in ("enabled", "max_concurrent"):
                    for value in (None, "garbage", "TRUE", "0", "1.5"):
                        self.config(self.service, key)
                        if value is None:
                            self.redis("HDEL", redis_key, field)
                        else:
                            self.redis("HSET", redis_key, field, value)
                        self.control(clear=True)
                        check(self.gateway(key=key)["status"] == 200, "内部故障阻断业务")
                        event = eventually(lambda: self.events("acquire", upstream_cluster=self.cluster(), api_key=key))[-1]
                        check(event["status"] == 500 and json.loads(event["body"]).get("reason") == "invalid_config", event)
                        check(self.leases(key=key) == 0 and self.leases(key="") == 0, "损坏配置产生计数")
                return "10种损坏配置内部500，网关业务200，不回退或创建租约"
            finally:
                self.delete(self.service, key);self.delete(self.service)
        self.run_case("GW-损坏规则fail-open", corrupt)

        def direct_response():
            self.control(clear=True)
            response = self.gateway(host="no-route.test")
            check(response["status"] == 404, response)
            check(not self.events("acquire"), "无转发目标仍调用了acquire")
            return "无匹配路由时保留网关404，不发送acquire"
        self.run_case("GW-无上游路由目标放行", direct_response)

        def upstream_errors():
            for status in (429, 500, 503):
                self.control(clear=True)
                response = self.gateway(key=self.key, data={"response_status": status})
                check(response["status"] == status and response["body"] == {"error": "upstream fixture error"}, response)
                events = self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key)
                check(events and json.loads(events[-1]["body"]).get("allowed") is True, events)
                self.idle()
            return "上游自身429/500/503及响应体原样透传，acquire明确允许"
        self.run_case("GW-上游错误原样透传", upstream_errors)

    def metric_cases(self):
        def json_usage():
            before = [self.metric(name, "basic_001") for name in
                      ("llm_prompt_tokens_total", "llm_completion_tokens_total")]
            check(self.gateway(key=self.key)["status"] == 200, "业务请求失败")
            self.idle()
            eventually(lambda: self.metric("llm_prompt_tokens_total", "basic_001") == before[0] + 11)
            check(self.metric("llm_completion_tokens_total", "basic_001") == before[1] + 7, "completion 增量不正确")
            return "Prometheus 原生 domain/uid 标签，prompt +11、completion +7"
        self.run_case("MET-非流式usage及原生标签", json_usage)

        def sse_usage():
            before = self.metric("llm_prompt_tokens_total", "basic_001")
            response = self.gateway(key=self.key, data={"stream": True})
            check(response["status"] == 200, response)
            frames = [json.loads(line[6:]) for line in response["raw"].splitlines()
                      if line.startswith("data: ") and line != "data: [DONE]"]
            received = next(frame["received"] for frame in frames if "received" in frame)
            check(received["body"].get("stream_options", {}).get("include_usage") is True, received)
            check(not any(k.lower() == "content-length" for k in received["headers"]), received)
            self.idle()
            eventually(lambda: self.metric("llm_prompt_tokens_total", "basic_001") == before + 11)
            return "上游实际收到 include_usage=true，旧 Content-Length 已移除，SSE usage +11"
        self.run_case("MET-SSE注入与统计", sse_usage)

        def valid_sse_errors():
            before = self.metric("llm_stream_parse_errors_total", "basic_001")
            response = self.gateway(key=self.key, data={"stream": True, "hold_seconds": 0.3})
            check(response["status"] == 200 and "[DONE]" in response["raw"], response)
            for line in response["raw"].splitlines():
                if line.startswith("data: ") and line != "data: [DONE]":
                    json.loads(line[6:])
            self.idle()
            after = self.metric("llm_stream_parse_errors_total", "basic_001")
            evidence = {"before": before, "after": after, "delta": after - before,
                        "response": response}
            self.save("valid-sse-errors.json", evidence)
            check(after == before, f"合法 SSE 全部帧可解析且正常 [DONE]，parse_errors 仍增加 {after - before} ({before}→{after})")
            return "合法 SSE 完成后 parse_errors 不增加"
        self.run_case("MET-合法SSE不误报错误", valid_sse_errors)

        def invalid_frame():
            before = self.metric("llm_stream_parse_errors_total", "basic_001")
            response = self.gateway(key=self.key, data={"stream": True, "invalid_frame": True})
            check(response["status"] == 200 and "[DONE]" in response["raw"], response)
            self.idle()
            eventually(lambda: self.metric("llm_stream_parse_errors_total", "basic_001") == before + 1)
            after = self.metric("llm_stream_parse_errors_total", "basic_001")
            self.save("invalid-sse-errors.json", {"before": before, "after": after,
                      "delta": after - before, "response": response})
            return {"before": before, "after": after, "delta": after - before,
                    "business_status": 200, "stream_done": True}
        self.run_case("MET-SSE非法帧", invalid_frame)

        def invalid_uid():
            self.config(self.service, "abc")
            before = sum(self.metric("llm_prompt_tokens_total", uid) for uid in ("basic_001", "premium_001", "__other__"))
            response = self.gateway(key="abc", data={"stream": True})
            check(response["status"] == 200, response)
            frame = next(json.loads(line[6:]) for line in response["raw"].splitlines()
                         if line.startswith("data: ") and '"received"' in line)
            check("stream_options" not in frame["received"]["body"], frame)
            self.idle(key="abc")
            after = sum(self.metric("llm_prompt_tokens_total", uid) for uid in ("basic_001", "premium_001", "__other__"))
            check(after == before, (before, after))
            return "非 JWT key 仍能限流及转发，同时关闭 usage 注入与统计"
        self.run_case("MET-无效uid旁路统计", invalid_uid)

        def cardinality():
            host = "overflow.e2e.test"
            self.config(self.service, "", 100)
            before = self.metric("llm_prompt_tokens_total", "__other__", host)
            for n in range(24):
                check(self.gateway(host=host, key=jwt(f"overflow_{n}"))["status"] == 200, n)
            eventually(lambda: self.metric("llm_prompt_tokens_total", "__other__", host) > before)
            return {"test_metric_key_limit": 8, "unique_uids": 24,
                    "overflow_prompt_tokens": self.metric("llm_prompt_tokens_total", "__other__", host)}
        self.run_case("MET-指标基数溢出", cardinality)

    def failure_cases(self):
        for mode in ("503", "500", "invalid", "timeout", "empty", "null", "missing_allowed",
                     "unknown_denial", "internal_error", "invalid_config", "api_key_disabled", "config_not_found", "oversize"):
            def fault(mode=mode):
                self.idle()
                self.control(acquire=mode, clear=True)
                try:
                    response = self.gateway(key=self.key)
                    check(response["status"] == 200 and response["body"].get("id") == "e2e-upstream", response)
                    check(self.leases() == 0, "故障 acquire 不应创建租约")
                    self.save("fault-acquire-" + mode + ".json", {"response": response,
                              "proxy": request(self.fixture, "/control")["body"]})
                    return f"/acquire {mode} 时真实 WASM 放行，业务 HTTP 200"
                finally:
                    self.control(acquire="normal")
            self.run_case("FAULT-acquire-" + mode, fault)

        def large_response():
            self.idle()
            response = self.gateway(key=self.key, data={"large_response": True})
            check(response["status"] == 200 and len(response["body"].get("padding", "")) == 2*1024*1024,
                  {"status": response["status"], "length": len(response["raw"])})
            self.idle()
            return "2MiB JSON 原样转发，统计超限不暂停业务响应"
        if not self.args.rate_limiter_only:
            self.run_case("FAULT-统计体积超限放行", large_response)

        for mode in ("503", "invalid", "timeout"):
            def release_fault(mode=mode):
                self.idle()
                self.control(release=mode, clear=True)
                try:
                    response = self.gateway(key=self.key)
                    check(response["status"] == 200, response)
                    event = eventually(lambda: self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key))[-1]
                    lease = json.loads(event["body"])["lease_id"]
                    eventually(lambda: self.redis("EXISTS", "rl:lease:" + lease) == "0")
                    # Expired ZSET members are removed by the next acquire, not
                    # by Redis key expiry alone. Verify that production path.
                    recovered = self.acquire(self.service, self.key)["body"]
                    check(recovered.get("allowed") is True and recovered.get("current_count") == 1, recovered)
                    self.release(recovered["lease_id"])
                    self.idle()
                    return f"/release {mode} 时业务响应完整，TTL 到期后新 acquire 可回收旧槽位"
                finally:
                    self.control(release="normal")
            self.run_case("FAULT-release-" + mode, release_fault)

        for mode in ("503", "invalid", "missing"):
            def renew_fault(mode=mode):
                self.idle()
                self.control(renew=mode, clear=True)
                try:
                    future = self.pool.submit(self.gateway, 0, None, self.key, {"stream": True, "hold_seconds": 3})
                    event = eventually(lambda: self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key))[-1]
                    lease = json.loads(event["body"])["lease_id"]
                    eventually(lambda: self.events("renew", lease_id=lease))
                    response = future.result(timeout=10)
                    check(response["status"] == 200 and "[DONE]" in response["raw"], response)
                    check(self.redis("EXISTS", "rl:lease:" + lease) == "0", "失败续约的租约未过期")
                    recovered = self.acquire(self.service, self.key)["body"]
                    check(recovered.get("allowed") is True and recovered.get("current_count") == 1,
                          {"expired_lease": lease, "next_acquire": recovered})
                    self.release(recovered["lease_id"])
                    self.idle()
                    self.save("fault-renew-" + mode + ".json", {
                        "expired_lease": lease, "next_acquire": recovered,
                        "stream_status": response["status"], "stream_done": "[DONE]" in response["raw"],
                        "proxy": request(self.fixture, "/control")["body"]})
                    return f"/renew {mode} 持续失败，原 SSE 保持到 [DONE]；过期后新 acquire 清理旧成员并恢复"
                finally:
                    self.control(renew="normal")
            self.run_case("FAULT-renew-" + mode, renew_fault)

        self.redis_cases()

    def redis_cases(self):
        def counter_response():
            started = time.monotonic()
            try:
                direct = self.acquire(self.service, self.key)
            except (OSError, http.client.HTTPException) as exc:
                detail = {"error": type(exc).__name__, "message": str(exc),
                          "seconds": round(time.monotonic() - started, 3)}
                self.save("redis-outage-counter-service.json", detail)
                raise AssertionError(f"Redis 停机时 /acquire 未返回约定的 HTTP 503：{detail}") from exc
            elapsed = time.monotonic() - started
            self.save("redis-outage-counter-service.json", {"response": direct, "seconds": round(elapsed, 3)})
            check(direct["status"] == 503 and direct["body"].get("allowed") is False and
                  direct["body"].get("reason") == "redis_unavailable", direct)
            check(elapsed < 0.8, f"/acquire 耗时 {elapsed:.3f}s，必须小于 800ms")
            return {"status": 503, "reason": "redis_unavailable", "seconds": round(elapsed, 3)}

        def gateway_response():
            started = time.monotonic()
            response = self.gateway(key=self.key)
            elapsed = time.monotonic() - started
            events = self.events("acquire", upstream_cluster=self.cluster(), api_key=self.key)
            self.save("redis-outage-gateway.json", {"response": response,
                      "seconds": round(elapsed, 3), "acquire_events": events})
            check(response["status"] == 200 and response["body"].get("id") == "e2e-upstream", response)
            check(events and events[-1]["status"] == 503 and
                  json.loads(events[-1]["body"]).get("reason") == "redis_unavailable", events)
            return {"business_status": 200, "acquire_status": 503, "seconds": round(elapsed, 3)}

        try:
            self.idle()
            self.control(clear=True)
            self.k("-n", self.ns, "scale", "deployment/redis", "--replicas=0")
            eventually(lambda: not json.loads(self.k("-n", self.ns, "get", "endpoints", "redis", "-o", "json")).get("subsets"), 20)
            # Endpoint 会先于进程退出被摘除；已有 Redis 连接仍可能继续工作。
            eventually(lambda: not json.loads(self.k("-n", self.ns, "get", "pods", "-l", "app=redis", "-o", "json"))["items"], 45)
            self.run_case("FAULT-Redis停机CounterService响应", counter_response)
            self.run_case("FAULT-Redis停机网关放行", gateway_response)

            def startup_without_redis():
                self.k("-n", self.ns, "rollout", "restart", "deployment/counter-service")
                self.k("-n", self.ns, "rollout", "status", "deployment/counter-service", "--timeout=60s", timeout=70)
                self.cs = self.forward("service/counter-service", 8080)[0]
                conn = http.client.HTTPConnection("127.0.0.1", self.cs, timeout=10)
                try:
                    conn.request("GET", "/wasm/rate-limiter.wasm")
                    response = conn.getresponse()
                    content = response.read()
                    expected = (Path(self.args.artifacts) / "rate-limiter.wasm").read_bytes()
                    check(response.status == 200 and content == expected, "Redis 停机时 Wasm 下载失败或内容不一致")
                finally:
                    conn.close()
                check(self.gateway(key=self.key)["status"] == 200, "重启 Counter Service 后业务未放行")
                return "Redis 停机时 Counter Service 可启动、Wasm 下载摘要一致，业务继续通过"
            self.run_case("FAULT-Redis不可用时冷启动下载服务", startup_without_redis)
        finally:
            self.k("-n", self.ns, "scale", "deployment/redis", "--replicas=1")
            self.k("-n", self.ns, "rollout", "status", "deployment/redis", "--timeout=90s", timeout=100)
            # 测试 Redis 使用临时数据，恢复后重新初始化以验证恢复路径。
            eventually(lambda: self.api("/config", {"service": self.service, "port": 8080, "api_key": self.key,
                "max_concurrent": 1, "enabled": True}, "PUT")["status"] == 200, 30)

        def recovery():
            check(self.gateway(key=self.key)["status"] == 200, "Redis 恢复后请求失败")
            self.idle()
            return "Redis 恢复后 acquire/release 正常，无遗留租约"
        self.run_case("FAULT-故障恢复", recovery)

    def loading_failure_cases(self):
        original = [item for item in self.manifest()["items"] if item["kind"] == "EnvoyFilter"]
        sequence = 0
        # During LDS/ECDS convergence the preceding healthy plugin can still
        # enforce its policy. Start from an explicit allow rule, not a missing rule.
        self.config(self.service, self.key, 100)
        check(self.gateway(key=self.key)["status"] == 200, "加载故障测试前的业务基线失败")
        self.idle()

        def apply_failure(mode):
            nonlocal sequence
            sequence += 1
            resources = json.loads(json.dumps(original))
            for resource in resources:
                plugin = resource["metadata"]["name"]
                for patch in resource["spec"]["configPatches"]:
                    value = patch["patch"]["value"]
                    name = f"{plugin}-fault-{sequence}"
                    if patch["applyTo"] == "HTTP_FILTER":
                        value["name"] = name
                    if patch["applyTo"] != "EXTENSION_CONFIG":
                        continue
                    value["name"] = name
                    cfg = value["typed_config"]["config"]
                    cfg["vm_config"]["vm_id"] = name
                    remote = cfg["vm_config"]["code"]["remote"]
                    if mode == "missing_cluster":
                        remote["http_uri"]["cluster"] = "missing-wasm-cluster"
                        remote["sha256"] = hashlib.sha256(name.encode()).hexdigest()
                    elif mode in ("connection_refused", "dns_failure"):
                        cluster = name + "-download"
                        remote["http_uri"]["cluster"] = cluster
                        remote["sha256"] = hashlib.sha256(name.encode()).hexdigest()
                        address = "127.0.0.1" if mode == "connection_refused" else f"missing.{self.ns}.svc.cluster.local"
                        resource["spec"]["configPatches"].append({"applyTo": "CLUSTER", "match": {"context": "GATEWAY"},
                            "patch": {"operation": "ADD", "value": {"name": cluster, "type": "STRICT_DNS",
                                "connect_timeout": "0.1s", "load_assignment": {"cluster_name": cluster,
                                    "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {"socket_address": {
                                        "address": address, "port_value": 1}}}}]}]}}}})
                    elif mode == "bad_sha":
                        remote["sha256"] = hashlib.sha256(name.encode()).hexdigest()
                    elif mode == "http_404":
                        remote["http_uri"]["uri"] += ".missing"
                        remote["sha256"] = hashlib.sha256(name.encode()).hexdigest()
                    elif mode in ("invalid_module", "http_timeout"):
                        path = "slow" if mode == "http_timeout" else "invalid"
                        remote["http_uri"].update({"uri": f"http://fixture.{self.ns}.svc.cluster.local:8080/wasm/{path}.wasm",
                                                   "cluster": "e2e-counter"})
                        remote["sha256"] = hashlib.sha256(b"not a wasm module").hexdigest()
                        if mode == "http_timeout":
                            remote["http_uri"]["timeout"] = "0.1s"
                    elif mode == "invalid_config":
                        cfg["configuration"]["value"] = "{"
                    elif mode == "missing_runtime":
                        cfg["vm_config"]["runtime"] = "envoy.wasm.runtime.missing"
                    else:
                        remote["http_uri"].update({"uri": f"http://fixture.{self.ns}.svc.cluster.local:8080/wasm/fault.wasm",
                                                   "cluster": "e2e-counter"})
                        remote["sha256"] = hashlib.sha256(Path(self.args.fault_wasm).read_bytes()).hexdigest()
                        cfg["configuration"]["value"] = mode
            self.k("apply", "-f", "-", data={"apiVersion": "v1", "kind": "List", "items": resources})
            return resources

        def assert_pass_through():
            for i in range(2):
                response = self.gateway(pod=i, key=self.key)
                check(response["status"] == 200 and response["body"].get("id") == "e2e-upstream", response)
            return True

        def logs():
            return "\n".join(self.k("-n", self.ns, "logs", pod, "--tail=300") for pod in self.pods)

        try:
            for mode in ("missing_cluster", "connection_refused", "dns_failure", "bad_sha", "http_404", "http_timeout",
                         "invalid_module", "invalid_config", "missing_runtime",
                         *(("fail_config", "trap_request", "trap_response") if self.args.fault_wasm else ())):
                def fault(mode=mode):
                    stop = threading.Event()
                    failures = []
                    def monitor():
                        while not stop.is_set():
                            for i in range(2):
                                try:
                                    status = self.gateway(pod=i, host="unmatched.test")["status"]
                                    if status != 200:
                                        failures.append({"pod": i, "status": status})
                                except Exception as exc:
                                    failures.append({"pod": i, "error": str(exc)})
                            stop.wait(0.05)
                    observing = self.pool.submit(monitor)
                    try:
                        resources = apply_failure(mode)
                        eventually(assert_pass_through, 30)
                        # Verify the trap was reached, not just the initial Lua fallback.
                        if mode in ("fail_config", "trap_request", "trap_response"):
                            marker = "failure fixture started: " + mode if mode == "fail_config" else "injecting " + mode
                            def injected():
                                assert_pass_through()
                                return marker in logs()
                            eventually(injected, 30)
                        else:
                            time.sleep(2)
                        assert_pass_through()
                        self.save("fault-load-" + mode + "-manifest.json", resources)
                        self.save("fault-load-" + mode + ".log", logs())
                        self.save("fault-load-" + mode + "-config.json", request(self.gw[0][2], "/config_dump")["body"])
                    finally:
                        stop.set()
                        observing.result(timeout=30)
                        self.save("fault-load-" + mode + "-traffic.json", failures)
                    check(not failures, failures[:10])
                    return "两个网关副本均透传到业务上游并返回 HTTP 200"
                self.run_case("FAULT-load-" + mode, fault)

            def cold_start():
                apply_failure("missing_cluster")
                self.k("-n", self.ns, "rollout", "restart", "deployment/e2e-gateway")
                self.k("-n", self.ns, "rollout", "status", "deployment/e2e-gateway", "--timeout=90s", timeout=100)
                pods = json.loads(self.k("-n", self.ns, "get", "pods", "-l", "app=e2e-gateway", "-o", "json"))["items"]
                self.pods = sorted(p["metadata"]["name"] for p in pods if not p["metadata"].get("deletionTimestamp"))
                self.gw = [self.forward("pod/" + p, 8080, 15090, 15000) for p in self.pods]
                eventually(assert_pass_through, 30)
                return "冷启动时 Wasm 下载 Cluster 不存在，网关仍就绪并透传业务"
            self.run_case("FAULT-load-冷启动无下载源", cold_start)
        finally:
            self.k("apply", "-f", "-", data={"apiVersion": "v1", "kind": "List", "items": original})
            self.assert_limiter_active()
            self.config(self.service, self.key, 1)

    def evidence(self):
        self.save("resources.json", json.loads(self.k("-n", self.ns, "get",
                  "pods,deployments,services,envoyfilters,gateways,virtualservices", "-o", "json")))
        self.save("events.txt", self.k("-n", self.ns, "get", "events", "--sort-by=.lastTimestamp"))
        self.save("proxy-events.json", request(self.fixture, "/control")["body"])
        self.save("metrics.prom", self.metrics())
        for i, pod in enumerate(self.pods):
            self.save(pod + ".log", self.k("-n", self.ns, "logs", pod, "--tail=1000"))
            self.save(pod + "-config.json", request(self.gw[i][2], "/config_dump")["raw"])
            self.save(pod + "-stats.txt", request(self.gw[i][2], "/stats?filter=wasm|e2e-counter")["raw"])
        self.save("counter-service.log", self.k("-n", self.ns, "logs", "deployment/counter-service", "--tail=1000"))

    def finish(self):
        self.pool.shutdown(wait=True)
        for process, log in self.forwards:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            log.close()
        if self.created and not self.args.keep:
            self.k("delete", "namespace", self.ns, self.peer_ns, "--ignore-not-found=true", "--wait=true", "--timeout=60s", timeout=70)
            self.cleaned = True
        after = self.inventory()
        self.save("cluster-after.json", after)
        before_by_key = {(i["kind"], i["namespace"], i["name"]): i for i in self.baseline}
        after_by_key = {(i["kind"], i["namespace"], i["name"]): i for i in after}
        self.run_case("ENV-原有资源配置保持", lambda: check(before_by_key == after_by_key,
                       "原有 Deployment/EnvoyFilter/ConfigMap 与测试前不同") or
                       "原有 Deployment/EnvoyFilter/ConfigMap 配置与副本数一致")

    def report(self):
        passed = sum(item["status"] == "PASS" for item in self.results)
        failed = len(self.results) - passed
        lines = ["# 现有 Kubernetes 集群端到端测试报告", "",
                 f"- 集群：`{self.args.context}`",
                 f"- API Server：`{getattr(self, 'api_server', '未获取')}`",
                 f"- 测试命名空间：`{self.ns}`",
                 f"- 当前代码镜像：`{self.args.image}`",
                 f"- 结果：{passed} 通过，{failed} 失败", "",
                 "使用现有 Istio 控制面、两个隔离网关副本、真实 WASM、Counter Service 和 Redis。",
                 "业务上游为可控 HTTP/SSE fixture；Counter Service 前置透明故障代理用于注入异常。",
                 ("限流专项：未部署或验收已废弃的 token-stats。" if self.args.rate_limiter_only else "包含已废弃 token-stats 的既有用例。"),
                 "测试 TTL=1500ms；测试环境不代表生产容量或长期稳定性验收。",
                 "", "| 用例 | 结果 | 证据 |", "| --- | --- | --- |"]
        for item in self.results:
            detail = str(item["detail"]).replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {item['name']} | {item['status']} | {detail} |")
        lines += ["", "原始断言和异常见 `results.json`；清单见 `manifest.json`；",
                  "指标见 `metrics.prom`；两个网关配置转储和日志、Counter Service 日志一并保留。",
                  "测试命名空间" + ("已删除。" if self.cleaned else "保留。" if self.args.keep else "未确认清理完成，请检查集群。")]
        failures = [item for item in self.results if item["status"] == "FAIL"]
        if failures:
            lines += ["", "## 未通过项", ""]
            for item in failures:
                lines.append(f"- {item['name']}：{item['detail']}")
        if any(item["name"] == "MET-合法SSE不误报错误" and item["status"] == "FAIL" for item in failures):
            lines += ["", "合法 SSE 的响应与指标前后值见 `valid-sse-errors.json`。",
                      "当前 `token-stats/internal/plugin/root.go` 的 SSE 响应体分支会在读取 body 失败时累加解析错误；",
                      "零字节结束回调可能触发这一路径，具体根因仍需在插件回调中进一步验证。本次没有修改业务代码。"]
        if any(item["name"] == "FAULT-Redis停机CounterService响应" and item["status"] == "FAIL" for item in failures):
            lines += ["", "Redis 停机时 Counter Service 的实际响应或连接异常见 `redis-outage-counter-service.json`，",
                      "同一故障下网关放行的独立证据见 `redis-outage-gateway.json`。",
                      "入口 HTTP 服务的 WriteTimeout 为 5s，Redis 客户端使用默认网络超时和重试参数；",
                      "两者的时限配合值得检查，当前证据只能确认 HTTP 503 契约未达成。"]
        self.save("report.md", "\n".join(lines) + "\n")
        print(f"REPORT {self.out / 'report.md'}: {passed} PASS, {failed} FAIL", flush=True)
        return bool(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", default="kind-kind",
                        help="本地 Kubernetes context（默认 kind-kind，不读取 current-context）")
    parser.add_argument("--namespace", default="rate-limiter-e2e-" + time.strftime("%Y%m%d-%H%M%S"))
    parser.add_argument("--image", required=True)
    parser.add_argument("--artifacts", required=True, help="本次镜像对应的 WASM 目录")
    parser.add_argument("--output", default="dist/e2e/" + time.strftime("%Y%m%d-%H%M%S"))
    parser.add_argument("--keep", action="store_true", help="保留隔离测试资源供调试")
    parser.add_argument("--fault-wasm", help="可选：故意初始化失败或 trap 的测试 WASM，仅用于隔离测试")
    parser.add_argument("--rate-limiter-only", action="store_true", help="仅部署和验证限流，跳过已废弃的统计功能")
    parser.add_argument("--loading-only", action="store_true", help="仅运行插件加载故障与冷启动测试")
    args = parser.parse_args()
    suite = Suite(args)
    try:
        suite.setup()
        if not args.loading_only:
            suite.counter_cases()
            suite.gateway_cases()
            if not args.rate_limiter_only:
                suite.metric_cases()
            suite.failure_cases()
        suite.loading_failure_cases()
        suite.evidence()
    except Exception as exc:
        suite.run_case("ENV-执行异常", lambda: (_ for _ in ()).throw(exc))
        if suite.created:
            for resource in ("pods", "events"):
                try:
                    suite.save("failure-" + resource + ".txt", suite.k("-n", suite.ns, "get", resource, "-o", "wide"))
                except Exception:
                    pass
            for deployment in ("e2e-gateway", "counter-service", "fixture"):
                try:
                    suite.save("failure-" + deployment + ".log", suite.k("-n", suite.ns, "logs",
                               "deployment/" + deployment, "--all-pods=true", "--tail=200"))
                except Exception:
                    pass
    finally:
        if hasattr(suite, "baseline"):
            try:
                suite.finish()
            except Exception as exc:
                suite.run_case("ENV-清理异常", lambda: (_ for _ in ()).throw(exc))
    return suite.report()


if __name__ == "__main__":
    sys.exit(main())
