package redis

import (
	"strings"
)

// Lua 脚本内容（直接来自设计文档，稍作格式整理）
const (
	acquireScript = `-- acquire_with_config.lua
-- KEYS[1]: config_key (rl:config:<domain>:<api_key>)
-- KEYS[2]: counter_key (rl:counter:<domain>:<api_key>)
-- KEYS[3]: lease_key (rl:lease:<lease_id>)
-- KEYS[4]: wildcard_config_key (rl:config:*.{parent_domain}:<api_key>)
-- KEYS[5]: global_config_key (rl:config:*::<api_key>)
-- ARGV[1]: lease_ttl_ms
-- ARGV[2]: lease_id

local function get_config(key)
    if not key or key == "" then
        return nil
    end
    local config = redis.call('HGETALL', key)
    if #config == 0 then
        return nil
    end
    local result = {}
    for i = 1, #config, 2 do
        result[config[i]] = config[i + 1]
    end
    return result
end

-- 按优先级查找配置：精确 > 通配符 > 全局
local config = get_config(KEYS[1])
if not config then
    config = get_config(KEYS[4])
end
if not config then
    config = get_config(KEYS[5])
end

-- 配置不存在
if not config then
    return cjson.encode({allowed = false, reason = "config_not_found", message = "No rate limit configuration found for this domain and api_key"})
end

-- 检查是否启用
if config.enabled ~= "true" then
    return cjson.encode({allowed = false, reason = "api_key_disabled", message = "API key is disabled"})
end

local max_concurrent = tonumber(config.max_concurrent)
if not max_concurrent or max_concurrent <= 0 then
    return cjson.encode({allowed = false, reason = "invalid_config", message = "Invalid max_concurrent configuration"})
end

-- 获取当前计数
local counter_key = KEYS[2]
local current = tonumber(redis.call('GET', counter_key) or 0)

-- 检查是否超限
if current >= max_concurrent then
    return cjson.encode({allowed = false, reason = "limit_exceeded", max_concurrent = max_concurrent, current_count = current, message = "Concurrent limit exceeded"})
end

-- 递增计数器
local new_count = redis.call('INCR', counter_key)

-- 创建租约
local lease_key = KEYS[3]
local lease_ttl_ms = tonumber(ARGV[1])
local lease_id = ARGV[2]
redis.call('SET', lease_key, counter_key, 'PX', lease_ttl_ms)

return cjson.encode({
    allowed = true,
    lease_id = lease_id,
    max_concurrent = max_concurrent,
    current_count = new_count,
    tier = config.tier or "default"
})
`

	releaseScript = `-- release_with_lease.lua
-- KEYS[1]: lease_key (rl:lease:<lease_id>)

local counter_key = redis.call('GET', KEYS[1])
if not counter_key then
    return cjson.encode({released = false, reason = "lease_not_found", message = "Lease not found or already expired"})
end

redis.call('DEL', KEYS[1])
local new_count = redis.call('DECR', counter_key)
if new_count < 0 then
    redis.call('SET', counter_key, 0)
    new_count = 0
end

return cjson.encode({released = true, current_count = new_count})
`

	listConfigsScript = `-- list_configs.lua
-- KEYS[1]: pattern (rl:config:*)
-- ARGV[1]: cursor
-- ARGV[2]: count

local cursor = ARGV[1]
local count = tonumber(ARGV[2]) or 100

local result = redis.call('SCAN', cursor, 'MATCH', KEYS[1], 'COUNT', count)
local new_cursor = result[1]
local keys = result[2]

local configs = {}
for i, key in ipairs(keys) do
    local config = redis.call('HGETALL', key)
    if #config > 0 then
        local item = { key = key }
        for j = 1, #config, 2 do
            item[config[j]] = config[j + 1]
        end
        table.insert(configs, item)
    end
end

return cjson.encode({cursor = new_cursor, configs = configs})
`
)

// GetAcquireScript 返回 acquire 脚本内容
func GetAcquireScript() string {
	return strings.TrimSpace(acquireScript)
}

func GetReleaseScript() string {
	return strings.TrimSpace(releaseScript)
}

func GetListConfigsScript() string {
	return strings.TrimSpace(listConfigsScript)
}
