-- Renewal never resurrects an expired identity. A new epoch gets a new token.
if ARGV[1] == 'renew' and redis.call('GET', KEYS[7]) ~= ARGV[2] then return -4 end
if ARGV[1] == 'create' and redis.call('EXISTS', KEYS[7]) ~= 0 then return -4 end
local now = redis.call('TIME')
local deadline = now[1] * 1000 + math.floor(now[2] / 1000) + tonumber(ARGV[3])
redis.call('SET', KEYS[7], ARGV[2], 'PX', ARGV[3])
redis.call('ZADD', KEYS[5], deadline, ARGV[2])
return 1
