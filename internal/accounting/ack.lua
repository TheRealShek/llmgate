#!lua flags=allow-oom
-- A stale writer cannot acknowledge work or publish health after losing its lease.
if redis.call('GET', KEYS[3]) ~= ARGV[1] then return 0 end
if ARGV[2] == 'failed' then
    redis.call('DEL', KEYS[4], KEYS[3])
    return 1
end
for i = 4, #ARGV, 3 do
    local id = ARGV[i]
    -- A final result may have arrived while Postgres was committing admission.
    if redis.call('HGET', KEYS[1], 'a:' .. id) == ARGV[i + 1]
        and redis.call('HGET', KEYS[1], 'f:' .. id) == ARGV[i + 2] then
        if string.match(ARGV[i + 2], '^%s*$') then
            redis.call('ZADD', KEYS[2], '+inf', id)
        else
            redis.call('HDEL', KEYS[1], 'a:' .. id, 'f:' .. id)
            redis.call('HDEL', KEYS[6], id)
            redis.call('ZREM', KEYS[2], id)
        end
    end
end
redis.call('SET', KEYS[4], 'healthy', 'PX', ARGV[3])
redis.call('DEL', KEYS[3])
return 1
