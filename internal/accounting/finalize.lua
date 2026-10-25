#!lua flags=allow-oom
-- Only overwrite already-reserved fixed-size values. Do not grow the backlog.
-- Active requests can finish after database health expires or admission fills.
local id = ARGV[1]
if redis.call('GET', KEYS[7]) ~= ARGV[5] or redis.call('HGET', KEYS[6], id) ~= ARGV[5] then return -4 end
if redis.call('HGET', KEYS[1], 'a:' .. id) ~= ARGV[2] then return -1 end
local old = redis.call('HGET', KEYS[1], 'f:' .. id)
if not old then return -1 end
if old ~= ARGV[4] and old ~= ARGV[3] then return -2 end
redis.call('HSET', KEYS[1], 'f:' .. id, ARGV[3])
local now = redis.call('TIME')
redis.call('ZADD', KEYS[2], now[1] * 1000 + math.floor(now[2] / 1000), id)
redis.call('SREM', KEYS[8], id)
return 1
