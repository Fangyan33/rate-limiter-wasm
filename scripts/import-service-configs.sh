#!/usr/bin/env bash
# 通过 Counter Service API 导入服务规则。输入为 JSON 数组，不执行数据迁移。
set -euo pipefail
COUNTER_SERVICE_URL="${COUNTER_SERVICE_URL:-http://localhost:8080}"
usage() {
  echo "用法: $0 -f rules.json [-u http://counter-service:8080]"
  echo 'rules.json 是规则数组，每项包含 service(namespace/name)、port、max_concurrent、enabled，以及可选 api_key、description。'
}
CONFIG_FILE=""
while getopts "f:u:h" opt; do
  case "$opt" in
    f) CONFIG_FILE="$OPTARG" ;;
    u) COUNTER_SERVICE_URL="$OPTARG" ;;
    h) usage; exit 0 ;;
    *) usage; exit 1 ;;
  esac
done
if [[ -z "$CONFIG_FILE" || ! -f "$CONFIG_FILE" ]]; then usage; exit 1; fi
command -v jq >/dev/null || { echo '需要 jq' >&2; exit 1; }
jq -e 'type == "array" and all(.[]; type == "object")' "$CONFIG_FILE" >/dev/null
count=0
while IFS= read -r rule; do
  # 不记录规则内容，避免泄漏明文 API Key；非2xx立即退出并报告错误。
  curl --fail-with-body --silent --show-error --max-time 10 \
    -X PUT "$COUNTER_SERVICE_URL/config" -H 'Content-Type: application/json' \
    --data-binary "$rule" >/dev/null
  count=$((count + 1))
done < <(jq -c '.[]' "$CONFIG_FILE")
echo "已导入 $count 条服务规则。"
