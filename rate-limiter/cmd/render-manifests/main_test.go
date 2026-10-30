package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func documents(t *testing.T, path string) []*yaml.Node {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var result []*yaml.Node
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, document.Content[0])
	}
}

func value(t *testing.T, node *yaml.Node, keys ...string) *yaml.Node {
	t.Helper()
	for _, key := range keys {
		node = field(node, key)
		if node == nil {
			t.Fatalf("missing %v", keys)
		}
	}
	return node
}

func TestGeneratedRelease(t *testing.T) {
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// 既有源清单与文档必须逐字节保持不变。
	originals := map[string][]byte{}
	for _, dir := range []string{"deploy", "simple", "docs"} {
		err := filepath.WalkDir(filepath.Join(source, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			originals[path], err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, namespace := range []string{"metering", "rate-limiter-test"} {
		t.Run(namespace, func(t *testing.T) {
			output := t.TempDir()
			for _, wasm := range []string{"rate-limiter-plugin.wasm", "rate-limiter-token-stats.wasm"} {
				if err := os.WriteFile(filepath.Join(output, wasm), []byte(wasm), 0644); err != nil {
					t.Fatal(err)
				}
			}
			image := "registry.example.com:5000/team/rate-limiter-counter-service:v1"
			if err := generate(source, output, namespace, image); err != nil {
				t.Fatal(err)
			}
			for old, name := range filenames {
				path := filepath.Join(output, "deploy/istio", name)
				for _, root := range documents(t, path) {
					metadata := field(root, "metadata")
					if metadata == nil { // 独立插件配置不是 Kubernetes 对象。
						if old == "rate-limiter-plugin-config.yaml" && value(t, root, "distributed_limit", "counter_service", "cluster").Value != "rate-limiter-counter-service" {
							t.Fatal("插件配置未引用新 Cluster")
						}
						continue
					}
					if old == "istio-mesh-config-example.yaml" {
						if value(t, metadata, "name").Value != "istio" || value(t, metadata, "namespace").Value != "istio-system" {
							t.Fatal("Istio 自身资源被更名")
						}
						continue
					}
					if !strings.HasPrefix(value(t, metadata, "name").Value, "rate-limiter-") || value(t, metadata, "namespace").Value != namespace {
						t.Fatalf("资源命名或 namespace 错误: %s", path)
					}
					if value(t, root, "kind").Value == "EnvoyFilter" {
						checkFilter(t, root, namespace)
					}
					if value(t, root, "kind").Value == "Deployment" {
						selector := value(t, root, "spec", "selector", "matchLabels", "app").Value
						if selector != value(t, root, "spec", "template", "metadata", "labels", "app").Value {
							t.Fatal("Deployment selector 不匹配")
						}
						container := value(t, root, "spec", "template", "spec", "containers").Content[0]
						expected := image
						if old == "redis.yaml" {
							expected = "redis:8.6.1"
							pod := value(t, root, "spec", "template", "spec")
							claim := value(t, value(t, pod, "volumes").Content[0], "persistentVolumeClaim", "claimName").Value
							if claim != "rate-limiter-redis-pvc" {
								t.Fatal("PVC 引用未更新")
							}
							secret := value(t, value(t, container, "env").Content[0], "valueFrom", "secretKeyRef", "name").Value
							if secret != "rate-limiter-redis-secret" {
								t.Fatal("Secret 引用未更新")
							}
						}
						if value(t, container, "image").Value != expected {
							t.Fatal("镜像名称错误")
						}
					}
				}
			}
			originalGateway := documents(t, filepath.Join(source, "deploy/istio/ai-gateway.yaml"))[0]
			gateway := documents(t, filepath.Join(output, "deploy/istio/rate-limiter-ai-gateway.yaml"))[0]
			ingress := value(t, gateway, "spec", "components", "ingressGateways").Content[0]
			if value(t, ingress, "namespace").Value != namespace || value(t, ingress, "label", "istio").Value != "rate-limiter-ai-gateway" || value(t, ingress, "label", "app").Value != "rate-limiter-ai-gateway" {
				t.Fatal("网关与 EnvoyFilter 的 namespace/selector 未同步")
			}
			originalIngress := value(t, originalGateway, "spec", "components", "ingressGateways").Content[0]
			for _, key := range []string{"nodeSelector", "tolerations"} {
				// 比较配置内容，忽略源清单和输出的 YAML 行号差异。
				a, _ := yaml.Marshal(value(t, originalIngress, "k8s", key))
				b, _ := yaml.Marshal(value(t, ingress, "k8s", key))
				if !bytes.Equal(a, b) {
					t.Fatal("节点调度约束被更名")
				}
			}
			for path := range originals {
				if !strings.HasPrefix(path, filepath.Join(source, "simple")+string(filepath.Separator)) || filepath.Ext(path) != ".yaml" {
					continue
				}
				relative, _ := filepath.Rel(source, path)
				before, after := documents(t, path), documents(t, filepath.Join(output, relative))
				for i, root := range after {
					if value(t, root, "metadata", "name").Value != value(t, before[i], "metadata", "name").Value || value(t, root, "metadata", "namespace").Value != namespace {
						t.Fatal("simple 示例名称或 namespace 错误")
					}
				}
			}
			if _, err := os.Stat(filepath.Join(output, "README.md")); err != nil {
				t.Fatal(err)
			}
		})
	}
	for path, before := range originals {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("源文件发生改变: %s", path)
		}
	}
}

func checkFilter(t *testing.T, root *yaml.Node, namespace string) {
	t.Helper()
	if value(t, root, "spec", "workloadSelector", "labels", "istio").Value != "rate-limiter-ai-gateway" {
		t.Fatal("EnvoyFilter selector 错误")
	}
	var extension, filter string
	for _, patch := range value(t, root, "spec", "configPatches").Content {
		item := value(t, patch, "patch", "value")
		switch value(t, patch, "applyTo").Value {
		case "EXTENSION_CONFIG":
			extension = value(t, item, "name").Value
			config := value(t, item, "typed_config", "config")
			if value(t, config, "fail_open").Value != "true" || value(t, config, "vm_config", "nack_on_code_cache_miss").Value != "true" {
				t.Fatal("加载 fail-open 保障丢失")
			}
			remote := value(t, config, "vm_config", "code", "remote")
			uri := value(t, remote, "http_uri", "uri").Value
			wasm := uri[strings.LastIndex(uri, "/")+1:]
			if !strings.HasPrefix(uri, "http://rate-limiter-counter-service."+namespace+".svc.cluster.local:8080/wasm/rate-limiter-") || value(t, remote, "http_uri", "cluster").Value != "rate-limiter-counter-service" {
				t.Fatal("Wasm 下载引用错误")
			}
			if value(t, remote, "sha256").Value != fmt.Sprintf("%x", sha256.Sum256([]byte(wasm))) {
				t.Fatal("Wasm SHA256 与制品不一致")
			}
		case "HTTP_FILTER":
			filter = value(t, item, "name").Value
			if value(t, item, "config_discovery", "apply_default_config_without_warming").Value != "true" {
				t.Fatal("ECDS 透传默认配置丢失")
			}
		}
	}
	if extension == "" || extension != filter {
		t.Fatal("ECDS 与 HTTP Filter 名称不一致")
	}
}

func TestInvalidOutputAndNames(t *testing.T) {
	source := t.TempDir()
	for _, args := range [][3]string{
		{source, "metering", "rate-limiter-counter-service:v1"},
		{t.TempDir(), "bad/name", "rate-limiter-counter-service:v1"},
		{t.TempDir(), "metering", "registry.example.com/team/counter-service:v1"},
	} {
		if err := generate(source, args[0], args[1], args[2]); err == nil {
			t.Fatalf("expected rejection: %v", args)
		}
	}
}
