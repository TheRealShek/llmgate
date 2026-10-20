-- One bucket per API key, holding request credits and their timestamp.
-- RedisStore.Take calls this after authentication.
-- KEYS[1] is the bucket key and ARGV[1] is requests per minute.
-- Redis runs the script atomically and returns {allowed, retry_ms}.

local bucket_key = KEYS[1]
local capacity = tonumber(ARGV[1])
local minute_ms = 60000

-- Redis' clock keeps every gateway on one timeline;
-- convert both parts to milliseconds.
local clock = redis.call('TIME')
local seconds = tonumber(clock[1])
local microseconds = tonumber(clock[2])
local now_ms = seconds * 1000 + math.floor(microseconds / 1000)

-- A missing key starts full.
local state = redis.call('HMGET', bucket_key, 'tokens', 'updated_ms')
local stored_credits = state[1]
local stored_updated_ms = state[2]

local credits = capacity
local clock_recovery_ms = 0

if stored_credits and stored_updated_ms then
    local last_updated_ms = tonumber(stored_updated_ms)
    stored_credits = tonumber(stored_credits)

    -- Clamp a backwards clock so it cannot mint credits or shorten retry time.
    clock_recovery_ms = math.max(0, last_updated_ms - now_ms)
    now_ms = math.max(now_ms, last_updated_ms)

    -- Refill elapsed time, capped at capacity, which also enforces a lowered limit.
    local elapsed_ms = now_ms - last_updated_ms
    local earned_credits = elapsed_ms * capacity / minute_ms
    credits = math.min(capacity, stored_credits + earned_credits)
end

-- Spend a credit, or report when the shortfall refills.
local allowed = 0
local retry_ms = 0
if credits >= 1 then
    credits = credits - 1
    allowed = 1
else
    local missing_credits = 1 - credits
    local refill_wait_ms = missing_credits * minute_ms / capacity
    retry_ms = math.max(1, math.ceil(clock_recovery_ms + refill_wait_ms))
end

-- Save denied requests too, so the next refill starts at this timestamp.
-- Keep enough significant digits to round-trip the credit balance.
redis.call('HSET', bucket_key,
    'tokens', string.format('%.17g', credits),
    'updated_ms', now_ms)

-- One idle minute refills to full, so expiry grants nothing extra.
local expiry_ms = math.ceil(clock_recovery_ms + minute_ms)
redis.call('PEXPIRE', bucket_key, expiry_ms)

return { allowed, retry_ms }
