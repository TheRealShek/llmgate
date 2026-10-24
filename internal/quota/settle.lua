-- The marker makes reconciliation idempotent, including a replay after a lost reply.
-- Unknown usage keeps the allowance charged but releases the pending marker.
local key = KEYS[1]
local day = tonumber(ARGV[1])
local marker = 'r:' .. ARGV[2]
local charge = tonumber(ARGV[3])
local state = redis.call('HMGET', key, 'day', 'used', marker)
if not state[1] or tonumber(state[1]) ~= day or not state[3] then return 0 end
local used = tonumber(state[2])
local reserved = tonumber(state[3])
if not used or not reserved or used < reserved or reserved < 1
    or used > 9007199254740991 or used ~= math.floor(used) or reserved ~= math.floor(reserved) then
    return redis.error_reply('invalid daily quota settlement')
end
if charge >= 0 then
    -- Saturation fails closed if a misbehaving backend exceeds exact accounting bounds.
    used = math.min(9007199254740991, used - reserved + charge)
    redis.call('HSET', key, 'used', string.format('%.0f', used))
end
redis.call('HDEL', key, marker)
return 1
