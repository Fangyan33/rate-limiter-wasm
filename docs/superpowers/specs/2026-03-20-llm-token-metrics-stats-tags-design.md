# LLM token metrics stats_tags redesign

## Summary

This design changes LLM token metrics from the current "dynamic values embedded in metric name and later recovered by Prometheus relabeling" approach to an Envoy stats tag extraction approach modeled after Higress. The goal is to keep the final Prometheus metric names unchanged while exposing `domain` and `uid` as first-class labels with `-` and `.` characters preserved.

Target Prometheus output:

```promql
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain", uid="sfe-platform"}
```

Note on label values: `domain` is the normalized host (lowercased, port stripped via `normalizeHost()`). `uid` is passed through `sanitizeMetricValue()` which preserves `-`, `.`, `_` and replaces only structurally unsafe characters (`;`, `=`, `|`, whitespace, control characters, non-ASCII). The labels are not raw request values but are close to the original for typical ASCII inputs.

## Problem statement

The current implementation defines metrics in the WASM plugin using names like:

```text
llm_prompt_tokens_total;domain=<domain>;uid=<uid>;
```

Prometheus then uses `metricRelabelings` to recover `domain` and `uid` from `__name__`.

This breaks for real values because Envoy stat name sanitization normalizes non `[a-zA-Z0-9_]` characters before Prometheus sees the metric name. In practice:

- `-` becomes `_`
- `.` becomes `_`
- delimiter characters such as `;` and `=` also become `_`

That causes two concrete failures in the current design:

1. Original `domain` / `uid` values cannot be reconstructed when they contain `-` or `.`.
2. Regex extraction in Prometheus is brittle and produced artifacts like a trailing `_` in `uid`.

The root issue is not the Prometheus regex itself. The issue is that extraction happens too late, after Envoy has already normalized the stat name.

## Design goals

1. Keep final Prometheus metric names unchanged:
   - `llm_prompt_tokens_total`
   - `llm_completion_tokens_total`
   - `llm_stream_parse_errors_total`
2. Expose `domain` and `uid` as first-class Prometheus labels.
3. Preserve `-` and `.` characters in label values.
4. Remove reliance on `ServiceMonitor.metricRelabelings` for recovering labels from `__name__`.
5. Keep the plugin-side change focused on metric naming only, without changing token accounting semantics.
6. Reuse Envoy `stats_config.stats_tags`, which has already been verified to work in the target Istio 1.13.5 cluster.

## Non-goals

1. No change to how `uid` is derived from requests.
2. No change to existing `sanitizeMetricValue` or `normalizeHost` behavior. Label values reflect the output of those functions, not raw request values.
3. No change to token statistics enablement, counters, or accounting behavior.
4. No expansion to new metric dimensions beyond `domain` and `uid`.
5. No custom Prometheus endpoint.

## Chosen approach

Use a structured internal Envoy stat name that embeds dynamic values between low-collision anchor segments, then configure Envoy `stats_tags` to extract `domain` and `uid` before the Prometheus exposition step.

### Anchor segment design

The anchor segments use double-underscore wrapping to minimize collision with real domain names and uid values:

- `__host0domain__` — marks the start of the domain value
- `__user0id__` — marks the start of the uid value

These anchors were chosen because:

1. Double-underscore prefix/suffix is extremely unlikely in real DNS names or user identifiers.
2. The `0` digit further reduces collision probability with natural text.
3. They are visually distinct and easy to identify in raw stat names during debugging.

### Stat name format

```text
llm.<metric>.__host0domain__.<domain>.__user0id__.<uid>
```

Examples:

```text
llm.prompt_tokens_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
llm.completion_tokens_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
llm.stream_parse_errors_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
```

After Envoy extracts tags and removes the tagged segments, the remaining stat names are:

```text
llm.prompt_tokens_total
llm.completion_tokens_total
llm.stream_parse_errors_total
```

Those become the final Prometheus metric names:

```text
llm_prompt_tokens_total
llm_completion_tokens_total
llm_stream_parse_errors_total
```

### Why this approach

Compared with keeping the current `;domain=...;uid=...;` encoding, this design moves extraction earlier in the pipeline to the Envoy stats layer, before irreversible name normalization destroys the original values.

Compared with a more Higress-like fully generic skeleton such as `llm.<dimension>.<value>...metric.<name>`, this layout is optimized for the explicit requirement to keep the final metric names unchanged.

Compared with introducing custom reversible escaping, this approach keeps the common case simple and readable because ordinary `domain` and `uid` values are preserved directly.

## Alternatives considered

### Alternative A: keep current stat format and switch only to stats_tags

Example:

```text
llm_prompt_tokens_total;domain=<domain>;uid=<uid>;
```

Rejected because it still relies on separators that Envoy normalizes aggressively. Even if Envoy tag extraction can be made to work, the shape is less robust and harder to reason about than a structured dotted stat name.

### Alternative B: use a more generic skeleton with explicit metric anchor

Example:

```text
llm.__host0domain__.<domain>.__user0id__.<uid>.__metric0name__.<metric>
```

Rejected because it makes the final exposed metric name harder to keep identical to the current names. The selected `llm.<metric>.__host0domain__.<domain>.__user0id__.<uid>` layout naturally collapses back to the current metric names after tag extraction.

### Alternative C: reversible encoding inside the plugin plus Prometheus relabel decoding

Rejected because it continues to make Prometheus relabeling responsible for label recovery. It solves character preservation, but not the underlying layering issue.

### Alternative D: simpler anchors without double-underscore wrapping

Example anchors: `host0domain`, `user0id` (without `__` wrapping).

Rejected because legitimate domain names could theoretically contain these substrings (e.g., `api.user0id.example.com`), which would cause silent mis-extraction of label values. The double-underscore wrapping makes collision with real DNS names and user identifiers practically impossible.

## Detailed design

### 1. Plugin metric naming

The metric definition logic in `internal/plugin/root.go` will stop building names in the current semicolon/tag style and instead build names with the structured dotted format.

Current metric definitions:

- `llm_prompt_tokens_total;domain=%s;uid=%s;`
- `llm_completion_tokens_total;domain=%s;uid=%s;`
- `llm_stream_parse_errors_total;domain=%s;uid=%s;`

New metric definitions:

- `llm.prompt_tokens_total.__host0domain__.%s.__user0id__.%s`
- `llm.completion_tokens_total.__host0domain__.%s.__user0id__.%s`
- `llm.stream_parse_errors_total.__host0domain__.%s.__user0id__.%s`

To avoid duplicated string construction logic, metric-name assembly should be centralized in a helper, conceptually:

```go
func buildMetricName(metric, domain, uid string) string {
    return fmt.Sprintf("llm.%s.__host0domain__.%s.__user0id__.%s", metric, domain, uid)
}
```

All three counter getters should call that helper.

### 2. Sanitization behavior

The existing `sanitizeMetricValue` behavior already preserves the important characters for this design:

- `.` is preserved
- `-` is preserved
- `_` is preserved

It replaces characters that would obviously break the stat name or are unsupported, such as:

- `;`
- `=`
- `|`
- whitespace and control characters
- non-ASCII runes

That means the sanitizer does not need a conceptual redesign for the new metric format. The design depends on Envoy tag extraction happening before the final Prometheus metric-name normalization, not on plugin-side reversible escaping.

### 3. Envoy stats_tags extraction

The ingress Envoy bootstrap config will be extended with `stats_config.stats_tags` rules for `domain` and `uid`.

#### Envoy TagSpecifier semantics

Envoy `stats_tags` uses RE2 regex (no lookahead/lookbehind). The capture group convention is:

- **Group 1**: the full segment to remove from the stat name.
- **Group 2** (if present): the tag value. If absent, group 1 is used as the value.

After all `TagSpecifier` rules have been applied, the remaining stat name (with all group-1 segments removed) becomes the final metric name.

#### Proposed extraction rules

```yaml
stats_config:
  use_all_default_tags: true
  stats_tags:
    - tag_name: uid
      regex: '(\.__user0id__\.(.+))$'
    - tag_name: domain
      regex: '(\.__host0domain__\.(.+))$'
```

#### Extraction order and reasoning

**Important**: `uid` must be extracted first, then `domain`. The order matters because each extraction removes its matched segment from the stat name before the next rule runs.

**Step 1 — uid extraction**

Input stat name:

```text
llm.prompt_tokens_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
```

Regex: `(\.__user0id__\.(.+))$`

- Group 1 (removed): `.__user0id__.sfe-platform`
- Group 2 (tag value): `sfe-platform`

Remaining stat name:

```text
llm.prompt_tokens_total.__host0domain__.llm-svc.domain
```

**Step 2 — domain extraction**

Input stat name (after uid removal):

```text
llm.prompt_tokens_total.__host0domain__.llm-svc.domain
```

Regex: `(\.__host0domain__\.(.+))$`

- Group 1 (removed): `.__host0domain__.llm-svc.domain`
- Group 2 (tag value): `llm-svc.domain`

Remaining stat name:

```text
llm.prompt_tokens_total
```

**Final Prometheus output:**

```promql
llm_prompt_tokens_total{domain="llm-svc.domain",uid="sfe-platform"}
```

This works correctly because:

1. `uid` extraction anchors to `.__user0id__.` and grabs everything to end of string.
2. After `uid` segment removal, `domain` extraction anchors to `.__host0domain__.` and grabs everything to end of string (which is now just the domain value).
3. The remaining stat name is clean: `llm.prompt_tokens_total`.

### 4. Istio / Envoy deployment configuration

The existing deployment manifest in `deploy/istio/rate-limiter-envoyfilter.yaml` already carries the WASM filter and counter-service cluster wiring. The new `stats_config.stats_tags` configuration should be added as a `BOOTSTRAP` patch in the same EnvoyFilter resource so the metric extraction rules remain versioned with the plugin deployment.

Important detail: for `applyTo: BOOTSTRAP`, `patch.value` must be the Bootstrap object fields directly, not wrapped inside an extra `bootstrap:` field. The verified form is:

```yaml
- applyTo: BOOTSTRAP
  match:
    context: GATEWAY
  patch:
    operation: MERGE
    value:
      stats_config:
        use_all_default_tags: true
        stats_tags:
          - tag_name: uid
            regex: '(\.__user0id__\.(.+))$'
          - tag_name: domain
            regex: '(\.__host0domain__\.(.+))$'
```

not:

```yaml
patch:
  operation: MERGE
  value:
    bootstrap:
      stats_config:
        ...
```

**Operational note**: `applyTo: BOOTSTRAP` patches affect the Envoy bootstrap configuration. Changes to bootstrap configuration require a gateway pod restart to take effect. A `kubectl rollout restart` of the ingressgateway deployment is required after applying or updating this EnvoyFilter.

### 5. Prometheus scraping configuration

The current `ServiceMonitor` uses `metricRelabelings` to recover `domain` and `uid` from `__name__`.

That logic should be removed entirely after the Envoy `stats_tags` based extraction is deployed.

The `ServiceMonitor` will keep only the ordinary scrape configuration for `/stats/prometheus`.

This simplifies the monitoring pipeline:

1. WASM emits structured Envoy stat names.
2. Envoy extracts `domain` and `uid` as tags.
3. Prometheus scrapes metrics that already contain the desired labels.
4. No label reconstruction from `__name__` is needed.

## Data flow

### Before

```text
WASM metric name
  -> Envoy sanitizes stat name
  -> Prometheus scrapes __name__
  -> ServiceMonitor metricRelabelings try to recover domain/uid
  -> original '-' and '.' already lost
```

### After

```text
WASM metric name: llm.<metric>.__host0domain__.<domain>.__user0id__.<uid>
  -> Envoy stats_tags extracts uid (removes .__user0id__.<uid>)
  -> Envoy stats_tags extracts domain (removes .__host0domain__.<domain>)
  -> remaining stat name: llm.<metric>
  -> Envoy normalizes '.' to '_' for Prometheus exposition
  -> Prometheus scrapes metric with native labels
  -> metricRelabelings no longer required
```

## Compatibility and migration

### Rollout order

The migration should follow this sequence to minimize risk:

1. Deploy the updated WASM plugin (new metric naming) and the BOOTSTRAP EnvoyFilter patch (stats_tags) together.
2. Restart the ingressgateway deployment (`kubectl -n istio-system rollout restart deploy/istio-ingressgateway`).
3. Verify `/stats/prometheus` output shows the expected metric names and labels.
4. Only after verification succeeds, remove the `metricRelabelings` from the `ServiceMonitor`.

This order ensures that if the new stats_tags extraction fails, the old metricRelabelings are still in place as a safety net (though they will not work correctly with the new stat name format, so this is primarily a "don't break unrelated metrics" safeguard).

### Query compatibility

The final Prometheus metric names remain unchanged, so queries that only reference the metric name continue to work unchanged.

Examples that should continue to work:

```promql
sum(rate(llm_prompt_tokens_total[5m]))
sum(rate(llm_completion_tokens_total[5m]))
```

### Label value migration

Queries and dashboards that filtered on Envoy-normalized values must be updated to use the plugin-processed values (i.e., the output of `normalizeHost()` for `domain` and `sanitizeMetricValue()` for `uid`).

Example before (Envoy-normalized, lossy):

```promql
llm_prompt_tokens_total{domain="llm_svc_domain", uid="sfe_platform_"}
```

Example after (plugin-processed, preserving `-` and `.`):

```promql
llm_prompt_tokens_total{domain="llm-svc.domain", uid="sfe-platform"}
```

### Grafana variables

Variables based on:

```promql
label_values(llm_prompt_tokens_total, domain)
label_values(llm_prompt_tokens_total, uid)
```

will start returning plugin-processed values (preserving `-` and `.`) instead of Envoy-normalized placeholders. This is expected and desired.

## Risks

### 1. Regex mistakes in stats_tags

If the tag extraction regex is wrong, the resulting Prometheus exposition may show:

- missing `domain` or `uid` labels
- malformed metric names
- leftover anchor segments in the metric name

This risk is manageable because `/stats/prometheus` and `istioctl proxy-config bootstrap` give direct evidence of the final rendered behavior.

### 2. Anchor collision inside values

The B1 design intentionally does not encode values. It assumes that real `domain` and `uid` values will not contain the chosen full anchor tokens in structurally conflicting ways.

The chosen anchors use double-underscore wrapping:

- `__host0domain__`
- `__user0id__`

This makes collision with real DNS names and user identifiers practically impossible because:

1. DNS labels cannot contain consecutive underscores in standard usage.
2. The `0` digit combined with double underscores creates a pattern that does not occur in natural identifiers.

**Engineering assumption**: this design accepts the constraint that `domain` and `uid` values must not contain the exact anchor substrings `__host0domain__` or `__user0id__`. The `sanitizeMetricValue()` function does not strip underscores, so theoretically a `uid` value could contain these patterns. In practice this is extremely unlikely for real user identifiers and DNS names. This is an accepted trade-off of the B1 (no-encoding) approach.

### 3. Existing dashboards depend on normalized label values

If current dashboards or alerts explicitly match normalized values such as `llm_svc_domain`, they will need to be updated to the original values after migration.

### 4. BOOTSTRAP patch requires gateway restart

Changes to `stats_config.stats_tags` via `applyTo: BOOTSTRAP` are not hot-reloaded. A gateway pod restart is required. This must be coordinated during deployment to avoid a window where the new plugin emits new stat names but the old bootstrap config lacks the extraction rules.

## Verification plan

### 1. Unit tests

Add or update tests in `internal/plugin/token_stats_test.go` around metric name generation so the three metric families are asserted against the new structured stat names.

Representative expected names:

```text
llm.prompt_tokens_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
llm.completion_tokens_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
llm.stream_parse_errors_total.__host0domain__.llm-svc.domain.__user0id__.sfe-platform
```

Edge cases to cover:

- `domain` containing `.` (e.g., `llm-svc.example.com`)
- `uid` containing `-` (e.g., `sfe-platform`)
- `uid` containing `.` (e.g., `user.name`)
- `metric_key_limit` overflow producing `uid="__other__"`

### 2. Bootstrap verification

Verify the gateway bootstrap contains the expected `stats_config.stats_tags` configuration:

```bash
istioctl proxy-config bootstrap <ingress-pod> -n istio-system -o json
```

Check for:

- `stats_config`
- `stats_tags`
- `tag_name: domain`
- `tag_name: uid`

### 3. Runtime Prometheus verification

Inspect the gateway admin metrics output:

```bash
kubectl -n istio-system exec <ingress-pod> -c istio-proxy -- \
  curl -s http://127.0.0.1:15090/stats/prometheus | grep '^llm'
```

Expected result shape:

```promql
llm_prompt_tokens_total{domain="llm-svc.domain",uid="sfe-platform"}
llm_completion_tokens_total{domain="llm-svc.domain",uid="sfe-platform"}
llm_stream_parse_errors_total{domain="llm-svc.domain",uid="sfe-platform"}
```

### 4. Prometheus query verification

Validate that grouped queries now operate directly on native labels:

```promql
sum by (domain, uid) (rate(llm_prompt_tokens_total[5m]))
```

### 5. Negative verification

Confirm that the old format is fully replaced:

- No metric names in `/stats/prometheus` output should contain `__host0domain__` or `__user0id__` as part of the metric name (they should only appear as label values extraction artifacts, not in the metric name itself).
- No metric names should contain the old `;domain=` or `;uid=` format.
- No metric names should have leftover anchor segments after tag extraction.

## Rollback

Rollback is straightforward because the change is concentrated in three places:

1. Revert the plugin metric naming change and redeploy the WASM binary.
2. Remove the `stats_tags` bootstrap patch from the EnvoyFilter.
3. Restore the old `ServiceMonitor.metricRelabelings` rules.
4. Restart the ingressgateway deployment.

That returns the system to the current behavior, including the existing normalization limitations.

## Implementation boundaries

This redesign should stay focused on the token metrics path only.

It should not be bundled with:

- distributed limiter changes
- unrelated EnvoyFilter cleanup
- auth changes
- token parsing changes
- additional metric dimensions

## Files expected to change

- `internal/plugin/root.go`
- `internal/plugin/token_stats_test.go`
- `deploy/istio/rate-limiter-envoyfilter.yaml`
- `deploy/istio/prometheus-servicemonitor.yaml`
