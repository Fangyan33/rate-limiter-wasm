#!/bin/bash
# migrate-config-to-redis.sh
# 将旧的 YAML rate_limits 配置迁移到 Redis（通过 Counter Service API）

set -e

COUNTER_SERVICE_URL="${COUNTER_SERVICE_URL:-http://localhost:8080}"
DEFAULT_DOMAIN="${DEFAULT_DOMAIN:-api.example.com}"

usage() {
  cat <<EOF
用法: $0 [选项]

将 YAML 格式的 rate_limits 配置迁移到 Redis。

选项:
  -f FILE       YAML 配置文件路径（默认: deploy/istio/rate-limiter-plugin-config.yaml）
  -u URL        Counter Service URL（默认: http://localhost:8080）
  -d DOMAIN     默认域名（默认: api.example.com）
  -h            显示此帮助信息

示例:
  # 使用默认配置文件
  $0

  # 指定配置文件和 Counter Service URL
  $0 -f config.yaml -u http://counter-service:8080

  # 指定默认域名
  $0 -d "*.example.com"

环境变量:
  COUNTER_SERVICE_URL  Counter Service URL
  DEFAULT_DOMAIN       默认域名
EOF
  exit 1
}

CONFIG_FILE="deploy/istio/rate-limiter-plugin-config.yaml"

while getopts "f:u:d:h" opt; do
  case $opt in
    f) CONFIG_FILE="$OPTARG" ;;
    u) COUNTER_SERVICE_URL="$OPTARG" ;;
    d) DEFAULT_DOMAIN="$OPTARG" ;;
    h) usage ;;
    *) usage ;;
  esac
done

if [ ! -f "$CONFIG_FILE" ]; then
  echo "错误: 配置文件不存在: $CONFIG_FILE"
  exit 1
fi

if ! command -v yq &> /dev/null; then
  echo "错误: 需要安装 yq 工具"
  echo "安装方法: https://github.com/mikefarah/yq"
  exit 1
fi

echo "开始迁移配置..."
echo "配置文件: $CONFIG_FILE"
echo "Counter Service: $COUNTER_SERVICE_URL"
echo "默认域名: $DEFAULT_DOMAIN"
echo ""

# 检查 Counter Service 是否可用
if ! curl -sf "$COUNTER_SERVICE_URL/health" > /dev/null; then
  echo "错误: Counter Service 不可用: $COUNTER_SERVICE_URL"
  exit 1
fi

# 提取 rate_limits 配置
rate_limits=$(yq eval '.rate_limits' "$CONFIG_FILE")

if [ "$rate_limits" = "null" ] || [ -z "$rate_limits" ]; then
  echo "警告: 配置文件中没有 rate_limits 配置"
  exit 0
fi

# 解析并导入每个配置
count=0
yq eval '.rate_limits[] | @json' "$CONFIG_FILE" | while read -r line; do
  api_key=$(echo "$line" | jq -r '.api_key')
  max_concurrent=$(echo "$line" | jq -r '.max_concurrent')
  tier=$(echo "$line" | jq -r '.tier // "standard"')

  echo "导入配置: domain=$DEFAULT_DOMAIN, api_key=$api_key, max_concurrent=$max_concurrent, tier=$tier"

  response=$(curl -s -w "\n%{http_code}" -X PUT "$COUNTER_SERVICE_URL/config" \
    -H "Content-Type: application/json" \
    -d "{
      \"domain\": \"$DEFAULT_DOMAIN\",
      \"api_key\": \"$api_key\",
      \"max_concurrent\": $max_concurrent,
      \"enabled\": true,
      \"tier\": \"$tier\"
    }")

  http_code=$(echo "$response" | tail -n1)
  body=$(echo "$response" | head -n-1)

  if [ "$http_code" = "200" ]; then
    echo "  ✓ 成功"
    count=$((count + 1))
  else
    echo "  ✗ 失败 (HTTP $http_code): $body"
  fi
done

echo ""
echo "迁移完成！共导入 $count 个配置"
echo ""
echo "验证配置:"
echo "  curl $COUNTER_SERVICE_URL/configs"
