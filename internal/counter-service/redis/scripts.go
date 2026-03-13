package redis

// acquireScript is the Lua script for atomic acquire operation
const acquireScript = `
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local lease_id = ARGV[4]

redis.call('ZREMRANGEBYSCORE', leases_key, '-inf', now_ms)
local current = redis.call('ZCARD', leases_key)
if current >= limit then
  return {0, ""}
end

local expire_at = now_ms + ttl_ms
redis.call('ZADD', leases_key, expire_at, lease_id)
redis.call('SET', lease_key, '1', 'PX', ttl_ms)
return {1, lease_id}
`

// releaseScript is the Lua script for atomic release operation
const releaseScript = `
local leases_key = KEYS[1]
local lease_key = KEYS[2]
local lease_id = ARGV[1]

if redis.call('EXISTS', lease_key) == 0 then
  return 0
end

redis.call('DEL', lease_key)
redis.call('ZREM', leases_key, lease_id)
return 1
`

// keyPrefix is the prefix for all Redis keys
const keyPrefix = "rl:concurrent:"

// buildLeasesKey builds the ZSET key for storing active leases
func buildLeasesKey(apiKey string) string {
	return keyPrefix + apiKey + ":leases"
}

// buildLeaseKey builds the lease key for an API key and lease ID
func buildLeaseKey(apiKey, leaseID string) string {
	return keyPrefix + apiKey + ":lease:" + leaseID
}
