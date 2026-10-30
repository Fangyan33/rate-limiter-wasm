// render-manifests 只生成发布副本，不修改仓库中的原始清单。
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var filenames = map[string]string{
	"counter-service-deployment.yaml": "rate-limiter-counter-service-deployment.yaml",
	"redis.yaml":                      "rate-limiter-redis.yaml",
	"ai-gateway.yaml":                 "rate-limiter-ai-gateway.yaml",
	"rate-limiter-envoyfilter.yaml":   "rate-limiter-plugin-envoyfilter.yaml",
	"rate-limiter-plugin-config.yaml": "rate-limiter-plugin-config.yaml",
	"token-stats-envoyfilter.yaml":    "rate-limiter-token-stats-envoyfilter.yaml",
	"token-stats-plugin-config.yaml":  "rate-limiter-token-stats-plugin-config.yaml",
	"prometheus-servicemonitor.yaml":  "rate-limiter-token-stats-servicemonitor.yaml",
	"istio-mesh-config-example.yaml":  "rate-limiter-istio-mesh-config-example.yaml",
}

func field(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func set(node *yaml.Node, key, value string) {
	if existing := field(node, key); existing != nil {
		existing.Value = value
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func rename(node *yaml.Node, namespace, image string, simple bool) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			// 调度标签属于已有基础设施，第三方镜像和 Istio 标准配置键保持原值。
			if key == "nodeSelector" || key == "tolerations" {
				continue
			}
			if key == "image" {
				if value.Value == "counter-service:latest" && !simple {
					value.Value = image
				}
				continue
			}
			rename(value, namespace, image, simple)
		}
		return
	}
	if node.Kind != yaml.ScalarNode {
		for _, child := range node.Content {
			rename(child, namespace, image, simple)
		}
		return
	}
	if simple {
		// 示例资源保留名称、域名、配置内容，只更新内部 Service 的跨命名空间引用。
		if node.Value == "mock-http-server.default.svc.cluster.local" {
			node.Value = "mock-http-server." + namespace + ".svc.cluster.local"
		}
		return
	}
	names := map[string]string{
		"counter-service":                  "rate-limiter-counter-service",
		"redis-deployment":                 "rate-limiter-redis",
		"redis-service":                    "rate-limiter-redis",
		"redis":                            "rate-limiter-redis",
		"redis-secret":                     "rate-limiter-redis-secret",
		"redis-pvc":                        "rate-limiter-redis-pvc",
		"redis-data":                       "rate-limiter-redis-data",
		"ai-gateway":                       "rate-limiter-ai-gateway",
		"istio-ingressgateway":             "rate-limiter-ai-gateway",
		"istio-ingressgateway-llm-metrics": "rate-limiter-token-stats-monitor",
		"rate-limiter":                     "rate-limiter-plugin",
		"token-stats":                      "rate-limiter-token-stats-wasm",
		"token-stats-wasm":                 "rate-limiter-token-stats-wasm",
		"token-stats-root":                 "rate-limiter-token-stats-root",
		"token-stats-vm":                   "rate-limiter-token-stats-vm",
		"ai-gateway-ns":                    namespace,
	}
	if name, ok := names[node.Value]; ok {
		node.Value = name
		return
	}
	node.Value = strings.NewReplacer(
		"counter-service.default.svc.cluster.local", "rate-limiter-counter-service."+namespace+".svc.cluster.local",
		"rate-limiter.wasm", "rate-limiter-plugin.wasm",
		"token-stats.wasm", "rate-limiter-token-stats.wasm",
		"cluster: counter-service", "cluster: rate-limiter-counter-service",
		"redis-service:6379", "rate-limiter-redis:6379",
	).Replace(node.Value)
}

func render(data []byte, namespace, image, digest string, simple bool) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(document.Content) == 0 {
			continue
		}
		root := document.Content[0]
		metadata := field(root, "metadata")
		if metadata != nil {
			// istio-system/istio 是控制面自身配置，不属于自有部署资源。
			platform := field(root, "kind").Value == "ConfigMap" && field(metadata, "name").Value == "istio"
			if !platform {
				rename(root, namespace, image, simple)
				set(metadata, "namespace", namespace)
				if !simple && field(root, "kind").Value == "IstioOperator" {
					gateways := field(field(field(root, "spec"), "components"), "ingressGateways")
					for _, gateway := range gateways.Content {
						set(field(gateway, "label"), "app", "rate-limiter-ai-gateway")
					}
				}
				if !simple && field(metadata, "name").Value == "rate-limiter-token-stats-wasm" {
					set(metadata, "name", "rate-limiter-token-stats")
				}
				if digest != "" {
					setDigest(root, digest)
				}
			}
		} else {
			rename(root, namespace, image, simple)
		}
		if err := encoder.Encode(&document); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), encoder.Close()
}

func setDigest(node *yaml.Node, digest string) {
	if node.Kind == yaml.MappingNode {
		if value := field(node, "sha256"); value != nil {
			value.Value = digest
		}
	}
	for _, child := range node.Content {
		setDigest(child, digest)
	}
}

func generate(source, output, namespace, image string) error {
	sourcePath, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	outputPath, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if sourcePath == outputPath {
		return fmt.Errorf("制品目录不能是仓库根目录，避免覆盖原始文件")
	}
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(namespace) || len(namespace) > 63 {
		return fmt.Errorf("非法 namespace: %q", namespace)
	}
	// registry 可以包含端口，校验最后一级镜像名即可。
	last := image[strings.LastIndex(image, "/")+1:]
	repository := strings.SplitN(last, ":", 2)[0]
	if !strings.HasPrefix(repository, "rate-limiter-") || repository == "rate-limiter-" {
		return fmt.Errorf("自有镜像的最后一级名称必须以 rate-limiter- 开头: %q", image)
	}
	for _, directory := range []string{"deploy/istio", "simple"} {
		err := filepath.WalkDir(filepath.Join(source, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			if filepath.Ext(path) != ".yaml" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			simple := directory == "simple"
			digest := ""
			if !simple {
				name, ok := filenames[entry.Name()]
				if !ok {
					return fmt.Errorf("清单尚未定义发布名称: %s", path)
				}
				relative = filepath.Join(directory, name)
				wasm := ""
				if entry.Name() == "rate-limiter-envoyfilter.yaml" {
					wasm = "rate-limiter-plugin.wasm"
				} else if entry.Name() == "token-stats-envoyfilter.yaml" {
					wasm = "rate-limiter-token-stats.wasm"
				}
				if wasm != "" {
					binary, err := os.ReadFile(filepath.Join(output, wasm))
					if err != nil {
						return err
					}
					digest = fmt.Sprintf("%x", sha256.Sum256(binary))
				}
			}
			data, err = render(data, namespace, image, digest, simple)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			destination := filepath.Join(output, relative)
			if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				return err
			}
			return os.WriteFile(destination, data, 0644)
		})
		if err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(output, "deploy/istio/rate-limiter-namespace.yaml"),
		[]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n"), 0644); err != nil {
		return err
	}
	readme := fmt.Sprintf(`# Rate Limiter 离线制品

默认命名空间：%s。镜像：%s。

校验及导入：

    sha256sum -c SHA256SUMS
    docker load -i rate-limiter-counter-service-image.tar

部署时依次使用 deploy/istio/ 下的 rate-limiter-namespace.yaml、rate-limiter-redis.yaml、
rate-limiter-counter-service-deployment.yaml 和 rate-limiter-plugin-envoyfilter.yaml。
网关示例是 rate-limiter-ai-gateway.yaml；复用已有网关时，应将 EnvoyFilter 放在目标网关的
命名空间，并按实际标签调整 workloadSelector。网关示例保留了原有节点调度约束，部署前需检查。
Redis 示例密码应在部署前配置。不要整体 apply 该目录：其中包含可选的 IstioOperator、
依赖监控 CRD 的 ServiceMonitor，以及仅供合并参考的 Istio 控制面配置示例。

二进制：rate-limiter-counter-service。
Wasm：rate-limiter-plugin.wasm、rate-limiter-token-stats.wasm。
token-stats 已废弃，保留构建产物和清单，不是限流部署的必要组件。
simple/ 保留示例名称，默认命名空间与本包一致；示例入口选择器仍需按实际网关调整。
docs/ 是保留的历史及源文档，其中的旧名称不代表本次生成物名称。
`, namespace, image)
	return os.WriteFile(filepath.Join(output, "README.md"), []byte(readme), 0644)
}

func main() {
	source := flag.String("source", ".", "仓库根目录")
	output := flag.String("output", "dist", "制品目录，需已有本次 Wasm")
	namespace := flag.String("namespace", "metering", "自有资源默认 namespace")
	image := flag.String("image", "rate-limiter-counter-service:latest", "Counter Service 镜像")
	flag.Parse()
	if err := generate(*source, *output, *namespace, *image); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
