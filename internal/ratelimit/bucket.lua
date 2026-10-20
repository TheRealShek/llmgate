-- One hash holds one key's request credits. Redis executes refill, admission,
-- consumption, and expiry atomically, using its own clock across gateway processes.
local rate = tonumber(ARGV[1])
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_ms')
local tokens = rate
local clock_delay = 0

if state[1] and state[2] then
    local updated = tonumber(state[2])
    clock_delay = math.max(0, updated - now)
    now = math.max(now, updated)
    tokens = math.min(rate, tonumber(state[1]) + (now - updated) * rate / 60000)
end

local allowed = 0
local retry_ms = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
else
    retry_ms = math.max(1, math.ceil(clock_delay + (1 - tokens) * 60000 / rate))
end

-- Preserve double precision on round trips, including unusually large limits.
redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'updated_ms', now)
-- Forget the bucket only after clock recovery and one full minute of refill.
redis.call('PEXPIRE', KEYS[1], math.ceil(clock_delay + 60000))
return {allowed, retry_ms}
