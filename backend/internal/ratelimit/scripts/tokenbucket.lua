-- Token bucket rate limiter.
--
-- A bucket refills at a steady rate up to a maximum. Each request takes one
-- token; a request arriving at an empty bucket is refused. This allows a
-- short burst up to the bucket's capacity while holding the long-run average
-- at the refill rate, which suits real traffic better than a fixed window:
-- a fixed window lets a client spend its whole allowance twice across the
-- boundary between two windows.
--
-- KEYS[1]  bucket key
-- ARGV[1]  capacity, the largest burst allowed
-- ARGV[2]  refill rate, tokens per second
-- ARGV[3]  now, as a unix timestamp in milliseconds
-- ARGV[4]  tokens this request costs
--
-- Returns { allowed, tokensRemaining, retryAfterMillis }

local capacity = tonumber(ARGV[1])
local refillPerSecond = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'updated')
local tokens = tonumber(bucket[1])
local updated = tonumber(bucket[2])

-- An unseen client starts with a full bucket.
if tokens == nil or updated == nil then
    tokens = capacity
    updated = now
end

-- Refill for the time that has passed, capped at capacity.
local elapsedSeconds = math.max(0, (now - updated) / 1000)
tokens = math.min(capacity, tokens + elapsedSeconds * refillPerSecond)

local allowed = 0
local retryAfter = 0

if tokens >= cost then
    allowed = 1
    tokens = tokens - cost
else
    -- How long until enough tokens have accrued for this request.
    retryAfter = math.ceil(((cost - tokens) / refillPerSecond) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated', now)

-- Expire idle buckets. A bucket is worthless once it has had time to refill
-- completely, so there is no need to keep it beyond that.
local ttl = math.ceil(capacity / refillPerSecond) + 60
redis.call('EXPIRE', KEYS[1], ttl)

return { allowed, math.floor(tokens), retryAfter }
