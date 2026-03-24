# LLM token metrics stats_tags redesign

## Summary

This design updates LLM token metrics to an Istio 1.13-compatible stats tag extraction model. The WASM plugin encodes `domain` and `uid` into the internal Envoy stat name using the delimiter pattern that Istio 1.13 already knows how to extract through `extraStatTags`, instead of relying on custom `stats_config.stats_tags` regex in an `EnvoyFilter` BOOTSTRAP patch.

The goal remains unchanged:

- keep the final Prometheus metric names unchanged
- expose `domain` and `uid` as first-class labels
- preserve `-` and `.` in label values
- remove `ServiceMonitor.metricRelabelings`

Target Prometheus output:

```promql
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```

Note on label values: `domain` is the normalized host (lowercased, port stripped via `normalizeHost()`). `uid` is passed through `sanitizeMetricValue()` which preserves `-`, `.`, `_` and replaces only structurally unsafe characters (`;`, `=`, `|`, whitespace, control characters, non-ASCII). The labels are not raw request values but are close to the original for typical ASCII inputs.

## Problem statement

The original implementation defined metrics in the WASM plugin using names like:

```text
llm_prompt_tokens_total;domain=<domain>;uid=<uid>;
```

Prometheus then used `metricRelabelings` to recover `domain` and `uid` from `__name__`.

That broke for real values because Envoy stat name sanitization normalized non `[a-zA-Z0-9_]` characters before Prometheus saw the metric name. In practice:

- `-` becomes `_`
- `.` becomes `_`
- delimiter characters such as `;` and `=` also become `_`

That caused two failures in the original design:

1. Original `domain` / `uid` values could not be reconstructed when they contained `-` or `.`.
2. Regex extraction in Prometheus was brittle and could produce artifacts.

A first redesign attempted to solve this with a custom dotted stat format and explicit Envoy `stats_config.stats_tags` regex configured via an `EnvoyFilter` BOOTSTRAP patch.

However, real-cluster verification on Istio 1.13.5 showed an important compatibility constraint:

1. The desired custom `stats_config.stats_tags` regex configured through `EnvoyFilter` BOOTSTRAP did not take effect as expected.
2. Istio 1.13 instead exposes stat tag extraction through mesh config `defaultConfig.extraStatTags`.
3. For tags such as `domain` and `uid`, Istio 1.13 uses built-in extraction patterns rather than the custom regex that was attempted in the `EnvoyFilter`.

So the problem is not just “extract labels before Prometheus.” The practical requirement is:

- use an internal stat-name shape that matches Istio 1.13’s built-in tag extraction behavior
- configure `extraStatTags` in mesh config
- avoid relying on custom BOOTSTRAP regex in `EnvoyFilter`

## Design goals

1. Keep final Prometheus metric names unchanged:
   - `llm_prompt_tokens_total`
   - `llm_completion_tokens_total`
   - `llm_stream_parse_errors_total`
2. Expose `domain` and `uid` as first-class Prometheus labels.
3. Preserve `-` and `.` characters in label values.
4. Remove reliance on `ServiceMonitor.metricRelabelings` for recovering labels from `__name__`.
5. Keep the plugin-side change focused on metric naming only, without changing token accounting semantics.
6. Make the deployment model match verified Istio 1.13.5 behavior.

## Non-goals

1. No change to how `uid` is derived from requests.
2. No change to existing `sanitizeMetricValue` or `normalizeHost` behavior. Label values reflect the output of those functions, not raw request values.
3. No change to token statistics enablement, counters, or accounting behavior.
4. No expansion to new metric dimensions beyond `domain` and `uid`.
5. No custom Prometheus endpoint.
6. No attempt to preserve a generic custom-regex Envoy bootstrap solution for all Istio versions.

## Chosen approach

Use the internal stat-name shape that works with Istio 1.13 built-in stat tag extraction for `domain` and `uid`, and configure those tags through mesh config `defaultConfig.extraStatTags`.

**Normative compatibility rule:** the target behavior depends on both of the following being true at the same time:

1. the plugin emits stat names in this exact structural shape:
   `llm.<metric>.;domain=.=<domain>;.;uid=.=<uid>;.;`
2. the Istio mesh config for the target gateway environment lists both `domain` and `uid` in `defaultConfig.extraStatTags`

If either condition is missing, the target `domain` / `uid` labels are not considered correctly implemented for Istio 1.13.5.

### Internal stat name format

```text
llm.<metric>.;domain=.=<domain>;.;uid=.=<uid>;.;
```

Examples:

```text
llm.prompt_tokens_total.;domain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
llm.completion_tokens_total.;domain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
llm.stream_parse_errors_total.;domain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
```

With mesh config:

```yaml
defaultConfig:
  extraStatTags:
    - domain
    - uid
```

Istio 1.13 extracts `domain` and `uid` from those internal stat names using its built-in tag handling, and the remaining exposed metric names stay equivalent to the existing Prometheus names.

### Why this approach

Compared with the original Prometheus relabel approach, this moves label extraction earlier to the Envoy/Istio stats layer, before Prometheus exposition.

Compared with the abandoned custom `stats_tags` regex design, this approach matches the actual behavior verified in a real Istio 1.13.5 cluster.

Compared with introducing a reversible custom encoding scheme, this keeps the metric name readable while preserving the characters that matter for `domain` and `uid` in normal cases.

## Alternatives considered

### Alternative A: keep Prometheus metricRelabelings

Rejected because it still recovers labels too late, after Envoy stat-name normalization has already lost important character information.

### Alternative B: custom dotted stat name plus custom BOOTSTRAP regex in EnvoyFilter

Example rejected shape:

```text
llm.<metric>.__host0domain__.<domain>.__user0id__.<uid>
```

Rejected because real-cluster verification on Istio 1.13.5 showed that the custom `stats_config.stats_tags` regex configured through `EnvoyFilter` BOOTSTRAP did not behave as needed. Keeping this design would leave the repository out of sync with the real deployment path.

### Alternative C: keep a dual-path design in docs

Rejected because the project’s target environment is explicitly Istio 1.13.5. Making the main design document describe both a “generic” custom-regex design and an Istio 1.13-compatible design would add confusion without helping the current deployment target.

## Detailed design

### 1. Plugin metric naming

The metric definition logic must produce names in the Istio 1.13-compatible format:

- `llm.%s.;domain=.=<domain>;.;uid=.=<uid>;.;`

One acceptable implementation is:

```go
func buildMetricName(metric, domain, uid string) string {
    return fmt.Sprintf("llm.%s.;domain=.=%s;.;uid=.=%s;.;", metric, domain, uid)
}
```

Any equivalent implementation is acceptable as long as all three counter families emit the same observable stat-name format.

Affected metric families:

- `prompt_tokens_total`
- `completion_tokens_total`
- `stream_parse_errors_total`

This change affects only metric naming, not token accumulation or request-flow behavior.

### 2. Sanitization behavior

The existing `sanitizeMetricValue` behavior remains appropriate, and in this design it is part of the compatibility contract rather than an arbitrary implementation detail.

It preserves:

- `.`
- `-`
- `_`

It replaces clearly unsafe structural characters such as:

- `;`
- `=`
- `|`
- whitespace and control characters
- non-ASCII runes

This matters because the internal stat-name contract depends on the delimiter structure remaining intact:

```text
;domain=.=<domain>;.;uid=.=<uid>;.;
```

So sanitizer changes are only acceptable if they preserve the ability of Istio 1.13 tag extraction to recognize that structure. In particular, sanitized values must not be allowed to inject or preserve delimiter characters that would corrupt the `domain` / `uid` segment boundaries.

### 3. Istio 1.13 tag extraction model

The deployment model changes from “configure custom regex through `EnvoyFilter` BOOTSTRAP” to “configure tag names through mesh config `extraStatTags`”.

Required mesh config shape:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: istio
  namespace: istio-system
data:
  mesh: |-
    defaultConfig:
      extraStatTags:
        - domain
        - uid
```

In the verified Istio 1.13.5 environment for this project:

- `extraStatTags` causes the bootstrap config to include `domain` and `uid` tag extractors
- those extractors use Istio-provided built-in regex behavior
- the custom regex values attempted in `EnvoyFilter` BOOTSTRAP are not the reliable control point for this version

So the design explicitly depends on:

1. plugin metric names following the expected internal delimiter shape
2. mesh config listing `domain` and `uid` in `extraStatTags`

### Mesh config example and repository alignment

For this design to be reproducible from repository artifacts, the repository should include or update an Istio mesh configuration example that shows:

```yaml
defaultConfig:
  extraStatTags:
    - domain
    - uid
```

This example may live in a dedicated deployment example file or in explicitly documented operator instructions, but it must exist as a maintained artifact in the repository if deployment examples are expected to be self-contained for Istio 1.13.

### EnvoyFilter deployment example

The repository `deploy/istio/rate-limiter-envoyfilter.yaml` should no longer include a `BOOTSTRAP` patch that attempts to define custom `stats_config.stats_tags` regex.

That example should focus only on:

- the `CLUSTER` patch for the counter-service upstream
- the `HTTP_FILTER` patch for the WASM filter insertion
- the inline plugin configuration

This keeps the EnvoyFilter example aligned with what is actually required for Istio 1.13.

### 5. Prometheus ServiceMonitor

`deploy/istio/prometheus-servicemonitor.yaml` no longer needs `metricRelabelings`.

Once the plugin metric name format and Istio tag extraction are aligned, Prometheus receives the final metric names and labels directly from Envoy’s `/stats/prometheus` output.

### 6. Expected final metrics

Expected Prometheus metrics remain:

```text
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```

## Testing strategy

### Plugin tests

Update `internal/plugin/token_stats_test.go` to assert the new internal stat-name format:

- `llm.prompt_tokens_total.;domain=.=api.example.com;.;uid=.=123;.;`
- `llm.completion_tokens_total.;domain=.=api.example.com;.;uid=.=123;.;`
- `llm.stream_parse_errors_total.;domain=.=api.example.com;.;uid=.=123;.;`

Also update the edge-case tests so they continue to verify that:

- domains containing `.` are preserved
- UIDs containing `-` are preserved
- UIDs containing `.` are preserved
- overflow to `__other__` still works with the new stat-name format

### Explicit deprecation checks

The repository should no longer present any of the following as the primary path for Istio 1.13:

- `__host0domain__`
- `__user0id__`
- `EnvoyFilter` BOOTSTRAP `stats_config.stats_tags` custom regex
- `ServiceMonitor.metricRelabelings`

These may be mentioned only as rejected or historical approaches. Tests, deployment examples, and implementation notes must not treat them as active architecture.

### Deployment config checks

- `deploy/istio/rate-limiter-envoyfilter.yaml` should be valid YAML after removing the BOOTSTRAP patch
- `deploy/istio/prometheus-servicemonitor.yaml` should remain valid YAML without `metricRelabelings`

### Live-cluster verification

For Istio 1.13 deployments, verification should be done in this order:

1. Ensure mesh config contains:
   - `extraStatTags: [domain, uid]`
2. Confirm ingress proxy bootstrap includes generated tag extractors for `domain` and `uid`
3. Deploy the updated WASM plugin
4. Check `/stats/prometheus` output from ingress Envoy
5. Confirm final metrics expose:
   - unchanged metric names
   - `domain` and `uid` labels with expected values
6. If temporary compatibility relabeling still exists in a live environment during rollout, remove it only after the new path is verified. It is not part of the target architecture.

## Migration / rollout notes

1. This design is written for the verified Istio 1.13.5 behavior in this project. Do not assume the same control points or extraction path automatically apply to other Istio versions.
2. Apply mesh config with `extraStatTags: [domain, uid]` before depending on the new metric labels in production dashboards.
3. Do not rely on the old `EnvoyFilter` BOOTSTRAP custom-regex patch in Istio 1.13.
4. Remove `ServiceMonitor.metricRelabelings` only after verifying the new metrics are present in the live cluster.
5. Dashboard and alert queries can continue to use the same metric names, while switching to native `domain` / `uid` labels.

## Files that must stay aligned

- `internal/plugin/root.go`
- `internal/plugin/token_stats_test.go`
- `deploy/istio/rate-limiter-envoyfilter.yaml`
- `deploy/istio/prometheus-servicemonitor.yaml`
- Istio mesh config example or deployment instructions that define `defaultConfig.extraStatTags: [domain, uid]`
- `docs/superpowers/specs/2026-03-20-llm-token-metrics-stats-tags-design.md`

If any one of these still reflects the abandoned `__host0domain__ / __user0id__ + custom regex` design, the repository becomes internally inconsistent.
