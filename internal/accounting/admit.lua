-- Reserve fixed admission and final-result slots before generation.
-- Health is a short-lived acknowledgment from the shared Postgres writer.
if redis.call('EXISTS', KEYS[4]) == 0 then return -1 end
if redis.call('GET', KEYS[7]) ~= ARGV[5] then return -4 end
local id = ARGV[1]
local admission = redis.call('HGET', KEYS[1], 'a:' .. id)
if admission then
    if admission ~= ARGV[2] or redis.call('HGET', KEYS[6], id) ~= ARGV[5] then return -2 end
    -- Produce a write on this connection so WAITAOF also covers identical retries.
    redis.call('HSET', KEYS[1], 'a:' .. id, admission)
    return 1
end
if redis.call('HLEN', KEYS[1]) >= tonumber(ARGV[4]) * 2 then return -3 end
redis.call('HSET', KEYS[1], 'a:' .. id, ARGV[2], 'f:' .. id, ARGV[3])
redis.call('HSET', KEYS[6], id, ARGV[5])
redis.call('SADD', KEYS[8], id)
local now = redis.call('TIME')
redis.call('ZADD', KEYS[2], now[1] * 1000 + math.floor(now[2] / 1000), id)
return 1
