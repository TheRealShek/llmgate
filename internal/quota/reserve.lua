-- RedisStore.Reserve admits against spent plus pending tokens on Redis' UTC day.
-- All validation precedes writes; no request can spend another request's allowance.
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ceiling = tonumber(ARGV[2])
local prompt = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])
local marker = 'r:' .. ARGV[5]
local seconds = tonumber(redis.call('TIME')[1])
local day = math.floor(seconds / 86400)
local reset = (day + 1) * 86400
local state = redis.call('HMGET', key, 'day', 'used')
local used = 0
local old_day = nil
if state[1] or state[2] then
    old_day = tonumber(state[1])
    used = tonumber(state[2])
    if not old_day or not used or old_day ~= math.floor(old_day) or old_day < 0 or old_day > day
        or used < 0 or used > 9007199254740991 or used ~= math.floor(used) then
        return redis.error_reply('invalid daily quota state')
    end
    if old_day < day then used = 0 end
end
local available = ceiling - used - prompt
if used >= limit or available < 1 then
    return {0, day, 0, reset - seconds}
end
local output = math.min(requested, available)
-- An older day's remaining markers cannot affect the new day's account.
if old_day and old_day < day then redis.call('DEL', key) end
local reserved = prompt + output
redis.call('HSET', key, 'day', day, 'used', string.format('%.0f', used + reserved),
    marker, string.format('%.0f', reserved))
redis.call('EXPIREAT', key, reset)
return {1, day, output, 0}
