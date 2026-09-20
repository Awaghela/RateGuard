-- Atomic token bucket.
--
-- KEYS[1]  bucket key
-- ARGV[1]  capacity        (max tokens / burst size)
-- ARGV[2]  refill rate     (tokens per second, may be fractional; 0 = never refills)
-- ARGV[3]  cost            (tokens to consume for this request)
--
-- Returns {allowed(0|1), tokens_remaining(string), retry_after_ms}
--
-- Time comes from Redis (TIME), not from the caller, so app instances with
-- skewed clocks cannot disagree about how many tokens have refilled.
-- The whole script runs atomically: no other command interleaves, so
-- concurrent callers on any number of app instances can never over-admit.

local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])

local t      = redis.call('TIME')
local now_ms = t[1] * 1000 + math.floor(t[2] / 1000)

local state  = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil or ts == nil then
  -- First sight of this client: start with a full bucket.
  tokens = capacity
  ts     = now_ms
end

-- Refill for elapsed time (clamped so a backwards clock step can't drain tokens).
local elapsed = math.max(0, now_ms - ts)
tokens = math.min(capacity, tokens + (elapsed * rate) / 1000)

local allowed  = 0
local retry_ms = 0
if tokens >= cost then
  tokens  = tokens - cost
  allowed = 1
elseif rate > 0 then
  retry_ms = math.ceil(((cost - tokens) * 1000) / rate)
else
  retry_ms = -1 -- will never refill
end

redis.call('HSET', key, 'tokens', tostring(tokens), 'ts', now_ms)

-- Idle buckets are indistinguishable from full ones, so let them expire:
-- time to fully refill, plus a 1s cushion.
local ttl_ms = 3600000
if rate > 0 then
  ttl_ms = math.ceil((capacity / rate) * 1000) + 1000
end
redis.call('PEXPIRE', key, ttl_ms)

return { allowed, tostring(tokens), retry_ms }
