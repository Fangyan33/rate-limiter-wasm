package redis

// acquireScript is the Lua script for atomic acquire operation
const acquireScript = `
local counter_key = KEYS[1]
local lease_key = KEYS[2]
local limit = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])

local current = tonumber(redis.call('GET', counter_key) or 0)
if current >= limit then
  return {0, ""}
end

redis.call('INCR', counter_key)
redis.call('SET', lease_key, '1', 'PX', ttl_ms)
return {1, lease_key}
`

// releaseScript is the Lua script for atomic release operation
const releaseScript = `
local counter_key = KEYS[1]
local lease_key = KEYS[2]

if redis.call('EXISTS', lease_key) == 1 then
  redis.call('DEL', lease_key)
  redis.call('DECR', counter_key)
  return 1
end
return 0
`

// keyPrefix is the prefix for all Redis keys
const keyPrefix = "rl:concurrent:"

// buildCounterKey builds the counter key for an API key
func buildCounterKey(apiKey string) string {
	return keyPrefix + apiKey + ":count"
}

// buildLeaseKey builds the lease key for an API key and lease ID
func buildLeaseKey(apiKey, leaseID string) string {
	return keyPrefix + apiKey + ":lease:" + leaseID
}
