# Istio 1.13 token metrics alignment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align the repository with the verified Istio 1.13.5 token-metrics design by keeping the new plugin metric-name format, updating tests, removing the obsolete EnvoyFilter BOOTSTRAP stats_tags patch, and adding maintained deployment guidance for `extraStatTags`.

**Architecture:** The plugin continues emitting internal stat names in the `llm.<metric>.;domain=.=<domain>;.;uid=.=<uid>;.;` shape. Istio 1.13.5 extracts `domain` and `uid` through mesh config `defaultConfig.extraStatTags`, so repository deployment examples must stop advertising custom EnvoyFilter BOOTSTRAP regex and instead document the mesh-config dependency explicitly. Tests and deployment artifacts must all reflect the same architecture.

**Tech Stack:** Go 1.22, Proxy-WASM Go SDK, Istio EnvoyFilter YAML, Kubernetes ConfigMap YAML, Go package tests

---

## File map

- Modify: `internal/plugin/token_stats_test.go`
  - Update all metric-name assertions from the abandoned `__host0domain__ / __user0id__` format to the verified Istio 1.13-compatible delimiter format.
- Modify if focused verification proves drift: `internal/plugin/root.go`
  - Ensure `buildMetricName` emits `llm.<metric>.;domain=.=<domain>;.;uid=.=<uid>;.;` only if the focused test shows it is still wrong.
- Modify: `deploy/istio/rate-limiter-envoyfilter.yaml`
  - Remove the obsolete `applyTo: BOOTSTRAP` custom `stats_config.stats_tags` patch.
- Modify if residue is found: `deploy/istio/prometheus-servicemonitor.yaml`
  - Keep the native-label path and remove `metricRelabelings` if any stale block remains.
- Modify: `deploy/istio/README.md`
  - Add explicit deployment guidance that Istio 1.13 requires mesh config `defaultConfig.extraStatTags: [domain, uid]`.
- Create: `deploy/istio/istio-mesh-config-example.yaml`
  - Add a maintained mesh-config example so the repository contains a concrete artifact for the required `extraStatTags` dependency.
- Verify alignment only: `docs/superpowers/specs/2026-03-20-llm-token-metrics-stats-tags-design.md`
  - Re-check that the final repository state still matches the approved spec wording.
- Do not stage or commit: `dist/rate-limiter.wasm`
  - This file is already dirty in the worktree and is not part of this plan.

---

### Task 1: Align plugin metric naming and token stats tests with the Istio 1.13 format

**Files:**
- Modify if needed: `internal/plugin/root.go`
- Modify: `internal/plugin/token_stats_test.go`

- [ ] **Step 1: Write the first failing expectation against the verified metric-name format**

Update `TestTokenStats_MetricNameFormat` to expect:

```text
llm.prompt_tokens_total.;domain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
llm.completion_tokens_total.;domain=.=llm-svc.domain;.;uid=.=sfe-platform;.;
```

- [ ] **Step 2: Run the focused test to verify the current behavior**

Run:

```bash
go test ./internal/plugin -run TestTokenStats_MetricNameFormat -count=1 -v
```

Expected:
- PASS if `internal/plugin/root.go` already emits the verified format
- FAIL with the actual metric name if `buildMetricName` still drifts from the spec

- [ ] **Step 3: If and only if Step 2 fails because `buildMetricName` still emits the wrong shape, make the minimal code change in `internal/plugin/root.go`**

Update the helper so that the observable output becomes exactly:

```text
llm.<metric>.;domain=.=<domain>;.;uid=.=<uid>;.;
```

Do not change any token-accounting or request-flow logic.

- [ ] **Step 4: Re-run the focused test to verify the helper now matches the spec**

Run:

```bash
go test ./internal/plugin -run TestTokenStats_MetricNameFormat -count=1 -v
```

Expected: PASS.

- [ ] **Step 5: Update the remaining token-stat tests to the new format**

Replace all remaining abandoned-format expectations in `internal/plugin/token_stats_test.go`, including at least:

- `TestTokenStats_MetricsIncrementedForUID`
- `TestTokenStats_MetricNamePreservesSpecialChars`
- `TestTokenStats_MetricNamePreservesDotsInUID`
- `TestTokenStats_MetricKeyLimitOverflowsToOther`
- `TestTokenStats_DisabledWhenJWTUIDMissing`
- `TestTokenStats_SSEParseErrorsIncrementedForInvalidJSONFrame`

Use expectations like:

```text
llm.prompt_tokens_total.;domain=.=api.example.com;.;uid=.=123;.;
llm.completion_tokens_total.;domain=.=api.example.com;.;uid=.=123;.;
llm.stream_parse_errors_total.;domain=.=api.example.com;.;uid=.=123;.;
llm.prompt_tokens_total.;domain=.=api.example.com;.;uid=.=__other__;.;
```

- [ ] **Step 6: Run the focused token-stats package tests**

Run:

```bash
go test ./internal/plugin -run TestTokenStats -count=1 -v
```

Expected: PASS with all token-stat tests green.

- [ ] **Step 7: Commit the plugin alignment changes**

Before staging, run:

```bash
git status --short
```

Confirm `dist/rate-limiter.wasm` is not staged.

If both files changed:

```bash
git add internal/plugin/root.go internal/plugin/token_stats_test.go
git commit -m "fix: align token metrics with Istio 1.13 stat naming"
```

If only tests changed:

```bash
git add internal/plugin/token_stats_test.go
git commit -m "test: align token stats assertions with Istio 1.13 metric names"
```

---

### Task 2: Remove the obsolete EnvoyFilter BOOTSTRAP stats_tags patch

**Files:**
- Modify: `deploy/istio/rate-limiter-envoyfilter.yaml`

- [ ] **Step 1: Write the failing deployment expectation as a repository diff target**

The file must no longer contain this structure:

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
            regex: ...
          - tag_name: domain
            regex: ...
```

The final file must no longer contain the obsolete `applyTo: BOOTSTRAP` custom-regex stats-tag patch. Leave any unrelated patches intact.

- [ ] **Step 2: Remove the BOOTSTRAP patch**

Delete the entire `applyTo: BOOTSTRAP` block from `deploy/istio/rate-limiter-envoyfilter.yaml` and do not modify the existing `CLUSTER` / `HTTP_FILTER` configuration except for the minimal YAML reflow required by deletion.

- [ ] **Step 3: Verify the YAML is still valid**

Run:

```bash
python3 -c "import yaml; yaml.safe_load(open('deploy/istio/rate-limiter-envoyfilter.yaml'))"
```

Expected: no output, exit code 0.

- [ ] **Step 4: Verify the obsolete BOOTSTRAP custom-regex patch is gone from the active EnvoyFilter**

Run:

```bash
grep -n "BOOTSTRAP\|stats_config\|stats_tags" deploy/istio/rate-limiter-envoyfilter.yaml
```

Expected: no matches in `deploy/istio/rate-limiter-envoyfilter.yaml`.

- [ ] **Step 5: Commit the EnvoyFilter cleanup**

Before staging, run:

```bash
git status --short
```

Confirm `dist/rate-limiter.wasm` is not staged.

```bash
git add deploy/istio/rate-limiter-envoyfilter.yaml
git commit -m "fix: remove obsolete Istio 1.13 bootstrap stats tags patch"
```

---

### Task 3: Re-check ServiceMonitor native-label alignment

**Files:**
- Verify and modify if needed: `deploy/istio/prometheus-servicemonitor.yaml`

- [ ] **Step 1: Check whether `metricRelabelings` is still present**

Run:

```bash
grep -n "metricRelabelings" deploy/istio/prometheus-servicemonitor.yaml
```

Expected: no matches.

- [ ] **Step 2: If residue exists, remove the stale relabeling block**

Only if Step 1 finds matches, remove the remaining `metricRelabelings` block so the file stays on the native-label path.

- [ ] **Step 3: Verify the ServiceMonitor YAML is valid**

Run:

```bash
python3 -c "import yaml; yaml.safe_load(open('deploy/istio/prometheus-servicemonitor.yaml'))"
```

Expected: no output, exit code 0.

- [ ] **Step 4: Commit the ServiceMonitor alignment if it changed**

Only if Step 2 changed the file.

Before staging, run:

```bash
git status --short
```

Confirm `dist/rate-limiter.wasm` is not staged.

```bash
git add deploy/istio/prometheus-servicemonitor.yaml
git commit -m "fix: keep ServiceMonitor on native token metric labels"
```

---

### Task 4: Add maintained Istio 1.13 mesh-config deployment artifacts

**Files:**
- Modify or add the most appropriate maintained deployment guidance file under `deploy/istio/`
- Create: `deploy/istio/istio-mesh-config-example.yaml`

- [ ] **Step 1: Add a failing documentation and artifact target**

The repository must contain a maintained deployment artifact that explicitly shows the Istio 1.13 mesh config in a complete `ConfigMap` shape, including:

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

The repository also needs a maintained operator-facing explanation that the EnvoyFilter example alone is not sufficient on Istio 1.13.5.

- [ ] **Step 2: Create the mesh-config example file**

Create `deploy/istio/istio-mesh-config-example.yaml` as a complete `ConfigMap` example for Istio 1.13 token metrics, not just a YAML fragment.

The file should show the real `data.mesh` embedding pattern with `defaultConfig.extraStatTags` and keep the example minimal beyond what is required for this dependency.

- [ ] **Step 3: Update the maintained deployment guidance**

Update the most appropriate maintained deployment guidance file under `deploy/istio/` (for example `deploy/istio/README.md`, if that remains the best fit) so that it explains:

1. the EnvoyFilter example alone is not sufficient for token metric labels on Istio 1.13.5
2. mesh config must list `domain` and `uid` in `defaultConfig.extraStatTags`
3. custom EnvoyFilter BOOTSTRAP regex is not the supported path for this version
4. Prometheus metric names stay unchanged while labels become native
5. `deploy/istio/istio-mesh-config-example.yaml` is the maintained example artifact for this dependency

- [ ] **Step 4: Verify active deployment guidance no longer instructs the abandoned custom-regex path**

Review the maintained guidance files you changed and confirm they do not present the old path as active deployment instructions.

Use targeted checks such as:

```bash
grep -n "stats_config\|stats_tags\|__host0domain__\|__user0id__" deploy/istio/README.md deploy/istio/istio-mesh-config-example.yaml
```

Expected:
- the maintained example artifact must not contain the abandoned custom-regex path
- maintained operator guidance must not recommend the abandoned path
- historical or rejected-approach explanations are allowed only if they are explicitly labeled as non-active guidance

- [ ] **Step 5: Verify the new YAML example is valid**

Run:

```bash
python3 -c "import yaml; yaml.safe_load(open('deploy/istio/istio-mesh-config-example.yaml'))"
```

Expected: no output, exit code 0.

- [ ] **Step 6: Commit the deployment guidance update**

Before staging, run:

```bash
git status --short
```

Confirm `dist/rate-limiter.wasm` is not staged.

```bash
git add deploy/istio/istio-mesh-config-example.yaml <the maintained guidance file you updated>
git commit -m "docs: add Istio 1.13 extraStatTags guidance for token metrics"
```

---

### Task 5: Final alignment verification for code, tests, and deploy artifacts

**Files:**
- Verify: `internal/plugin/root.go`
- Verify: `internal/plugin/token_stats_test.go`
- Verify: `deploy/istio/rate-limiter-envoyfilter.yaml`
- Modify if residue is found: `deploy/istio/prometheus-servicemonitor.yaml`
- Verify: `deploy/istio/README.md`
- Verify: `deploy/istio/istio-mesh-config-example.yaml`

- [ ] **Step 1: Run the focused plugin package tests**

Run:

```bash
go test ./internal/plugin -count=1
```

Expected: PASS.

- [ ] **Step 2: Run the full repository test suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 3: Verify no abandoned metric-name format remains in active code/tests/deploy docs**

Run:

```bash
grep -R -n "__host0domain__\|__user0id__" internal/plugin deploy/istio
```

Expected: no matches in active implementation/tests/deployment guidance.

- [ ] **Step 4: Verify active deploy artifacts no longer use the abandoned custom-regex path**

Check the active deployment artifacts you changed and confirm they do not instruct the old path. Historical or rejected-approach explanations are allowed only if they are clearly labeled as non-active guidance.

For example:

```bash
grep -n "stats_config\|stats_tags" deploy/istio/rate-limiter-envoyfilter.yaml deploy/istio/README.md deploy/istio/istio-mesh-config-example.yaml
```

Expected:
- `deploy/istio/rate-limiter-envoyfilter.yaml` must not contain the abandoned custom-regex path
- the maintained mesh-config example must not contain the abandoned custom-regex path
- maintained operator guidance must not recommend the abandoned path
- any remaining mention in prose must be clearly marked as rejected/historical context

- [ ] **Step 5: Re-check ServiceMonitor for residue and verify YAML validity**

Run:

```bash
grep -n "metricRelabelings" deploy/istio/prometheus-servicemonitor.yaml
```

Expected: no matches.

Then run:

```bash
python3 -c "import yaml; yaml.safe_load(open('deploy/istio/prometheus-servicemonitor.yaml'))"
```

Expected: no output, exit code 0.

If the grep command finds residue, remove the stale `metricRelabelings` block first, then re-run both checks and create a final cleanup commit.

- [ ] **Step 6: Record the live-cluster verification handoff checklist in the final report**

The final execution report must explicitly remind the user that repository changes alone do not activate labels in the cluster. The handoff must state that operators still need to:

1. apply mesh config containing `extraStatTags: [domain, uid]`
2. ensure the target ingress gateway picks up the updated mesh/bootstrap configuration
3. confirm ingress bootstrap shows generated tag extractors for `domain` and `uid`
4. deploy the updated WASM plugin
5. verify `/stats/prometheus` exposes unchanged metric names with native `domain` / `uid` labels
6. if any live-cluster compatibility `metricRelabelings` is still present outside this repository, remove it only after the new native-label path has been validated in the real cluster

This step is a handoff checklist, not a local command.

- [ ] **Step 7: Re-check spec alignment against the approved design doc**

Re-read:

- `docs/superpowers/specs/2026-03-20-llm-token-metrics-stats-tags-design.md`

Confirm the final repository state still matches the approved spec for:

- plugin metric-name shape
- EnvoyFilter deployment path
- ServiceMonitor native-label path
- mesh-config example presence

- [ ] **Step 8: Verify only intended files are staged for this alignment work**

Run:

```bash
git status --short
```

Expected:
- changes only in the files from this plan
- `dist/rate-limiter.wasm` may still appear dirty in the worktree, but it must not be staged or committed as part of this plan unless the user explicitly asks
- only plan-scoped files are staged before each commit and at final handoff

- [ ] **Step 9: Commit any final cleanup if needed**

Only if verification required a final small edit. Otherwise skip this step.

---

## Notes for implementers

- Use @superpowers:test-driven-development for the test changes in Task 1.
- Use @superpowers:verification-before-completion before claiming any task is done.
- Do not rewrite `internal/plugin/root.go` unless verification shows the already-present metric-name format is wrong.
- Do not change `normalizeHost` or `sanitizeMetricValue` semantics as part of this alignment work unless a focused failing test proves they are incompatible with the approved Istio 1.13 design.
- Do not touch unrelated distributed-limiter or counter-service logic.
- Do not commit `dist/rate-limiter.wasm` as part of this alignment work unless the user explicitly requests a rebuilt artifact.
