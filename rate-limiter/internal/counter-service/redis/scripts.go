package redis

import (
	"strings"
)

// Lua 脚本内容（直接来自设计文档，稍作格式整理）
const (
	acquireScript = `-- acquire_with_config.lua (ZSET model)
-- KEYS[1]: config_key        (rl:config:<domain>:<api_key_sha256>)
-- KEYS[2]: leases_key        (rl:leases:<domain>:<api_key_sha256>)
-- KEYS[3]: lease_key         (rl:lease:<lease_id>)
-- KEYS[4]: wildcard_config_key
-- KEYS[5]: global_config_key
-- KEYS[6]: domain_config_key (empty API key hash)
-- KEYS[7]: wildcard_domain_config_key
-- KEYS[8]: global_domain_config_key
-- KEYS[9]: domain_leases_key (actual request domain, empty API key hash)
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

-- Resolve domain rules first, and select their shared counter atomically.
local config = get_config(KEYS[6])
if not config then
    config = get_config(KEYS[7])
end
if not config then
    config = get_config(KEYS[8])
end
local leases_key = KEYS[9]
if not config then
    leases_key = KEYS[2]
    config = get_config(KEYS[1])
    if not config then
        config = get_config(KEYS[4])
    end
    if not config then
        config = get_config(KEYS[5])
    end
end

if not config then
    return cjson.encode({allowed = false, reason = "config_not_found", message = "No rate limit configuration found"})
end

if config.enabled ~= "true" then
    return cjson.encode({allowed = false, reason = "api_key_disabled", message = "API key is disabled"})
end

local max_concurrent = tonumber(config.max_concurrent)
if not max_concurrent or max_concurrent <= 0 then
    return cjson.encode({allowed = false, reason = "invalid_config", message = "Invalid max_concurrent configuration"})
end

local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', leases_key, '-inf', now_ms)

local current = redis.call('ZCARD', leases_key)
if current >= max_concurrent then
    return cjson.encode({allowed = false, reason = "limit_exceeded", max_concurrent = max_concurrent, current_count = current, message = "Concurrent limit exceeded"})
end

local lease_ttl_ms = tonumber(ARGV[1])
local lease_id = ARGV[2]
local expire_at = now_ms + lease_ttl_ms
redis.call('ZADD', leases_key, expire_at, lease_id)
redis.call('SET', KEYS[3], leases_key, 'PX', lease_ttl_ms)

local new_count = current + 1
return cjson.encode({
    allowed = true,
    lease_id = lease_id,
    max_concurrent = max_concurrent,
    current_count = new_count,
    tier = config.tier or "default"
})
`

	releaseScript = `-- release_with_lease.lua (ZSET model)
-- KEYS[1]: lease_key  (rl:lease:<lease_id>)
-- ARGV[1]: lease_id

local leases_key = redis.call('GET', KEYS[1])
if not leases_key then
    return cjson.encode({released = false, reason = "lease_not_found", message = "Lease not found or already expired"})
end

redis.call('DEL', KEYS[1])
redis.call('ZREM', leases_key, ARGV[1])
local current = redis.call('ZCARD', leases_key)

return cjson.encode({released = true, current_count = current})
`

	renewScript = `-- renew_with_lease.lua (ZSET model)
-- KEYS[1]: lease_key (rl:lease:<lease_id>)
-- ARGV[1]: lease_id
-- ARGV[2]: lease_ttl_ms

local leases_key = redis.call('GET', KEYS[1])
if not leases_key then
    return cjson.encode({renewed = false, reason = "lease_not_found"})
end

local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local score = redis.call('ZSCORE', leases_key, ARGV[1])
if not score or tonumber(score) <= now_ms then
    redis.call('DEL', KEYS[1])
    redis.call('ZREM', leases_key, ARGV[1])
    return cjson.encode({renewed = false, reason = "lease_not_found"})
end

-- A repeated renew must never shorten a lease that was already extended.
local expire_at = math.max(tonumber(score), now_ms + tonumber(ARGV[2]))
redis.call('ZADD', leases_key, 'XX', expire_at, ARGV[1])
redis.call('PEXPIRE', KEYS[1], expire_at - now_ms)
return cjson.encode({renewed = true})
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

func GetRenewScript() string {
	return strings.TrimSpace(renewScript)
}

func GetListConfigsScript() string {
	return strings.TrimSpace(listConfigsScript)
}
