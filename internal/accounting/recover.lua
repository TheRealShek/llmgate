#!lua flags=allow-oom
-- The owner key cannot be resurrected, so this fence remains valid after recovery.
if redis.call('EXISTS', KEYS[7]) ~= 0 then return -4 end
local now = redis.call('TIME')
local score = now[1] * 1000 + math.floor(now[2] / 1000)
for i = 2, #ARGV, 4 do
    local id = ARGV[i]
    if redis.call('HGET', KEYS[6], id) == ARGV[1]
        and redis.call('HGET', KEYS[1], 'a:' .. id) == ARGV[i + 1]
        and redis.call('HGET', KEYS[1], 'f:' .. id) == ARGV[i + 2] then
        if string.match(ARGV[i + 2], '^%s*$') then
            redis.call('HSET', KEYS[1], 'f:' .. id, ARGV[i + 3])
            redis.call('ZADD', KEYS[2], score, id)
        end
        redis.call('SREM', KEYS[8], id)
    end
end
return 1
