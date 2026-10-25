#!lua flags=allow-oom
local now = redis.call('TIME')
local milliseconds = now[1] * 1000 + math.floor(now[2] / 1000)
local owners = redis.call('ZRANGEBYSCORE', KEYS[5], '-inf', milliseconds, 'LIMIT', 0, 1)
if #owners == 0 then return {} end
local owner = owners[1]
if redis.call('EXISTS', ARGV[1] .. ':owner:' .. owner) ~= 0 then return {} end
local ids = redis.call('SRANDMEMBER', ARGV[1] .. ':requests:' .. owner, tonumber(ARGV[2]))
if #ids == 0 then
    redis.call('ZREM', KEYS[5], owner)
    return {}
end
local result = {owner}
for _, id in ipairs(ids) do table.insert(result, id) end
return result
