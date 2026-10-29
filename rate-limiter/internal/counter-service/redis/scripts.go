package redis

import (
	"strings"
)

// Atomic policy selection, lease accounting and lifecycle scripts.
const (
	acquireScript = `-- acquire_with_config.lua (ZSET model)
-- KEYS[1]: specific service-port config (empty when there is no API key)
-- KEYS[2]: specific service-port leases
-- KEYS[3]: lease pointer
-- KEYS[4]: service-port default config (empty API key hash)
-- KEYS[5]: service-port default leases
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

-- Resolve specific-key rules first, then the service-port default. Disabled entries are skipped.
local config = nil
local leases_key = nil
for _, index in ipairs({1, 4}) do
    local candidate = get_config(KEYS[index])
    if candidate then
        if candidate.enabled ~= "true" and candidate.enabled ~= "false" then
            return cjson.encode({allowed = false, reason = "invalid_config", message = "Invalid enabled configuration"})
        end
        if candidate.enabled == "true" then
            local raw_limit = candidate.max_concurrent or ""
            local limit = tonumber(raw_limit)
            if not string.match(raw_limit, '^[1-9][0-9]*$') or not limit or limit == math.huge then
                return cjson.encode({allowed = false, reason = "invalid_config", message = "Invalid max_concurrent configuration"})
            end
            config = candidate
            leases_key = index == 1 and KEYS[2] or KEYS[5]
            break
        end
    end
end

-- No applicable policy is a normal unmetered success, not a failed acquire.
if not config then
    return cjson.encode({allowed = true})
end
local max_concurrent = tonumber(config.max_concurrent)

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
    current_count = new_count
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
-- KEYS[1]: pattern (rl:service:config:*)
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
