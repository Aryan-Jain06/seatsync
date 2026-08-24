-- Read the seats currently held for an event.
--
-- KEYS[1]  reserved-set key for the event
-- ARGV[1]  now, as a unix timestamp in milliseconds
-- ARGV[2]  lock key prefix for the event, "lock:<eventId>:"
--
-- Returns a flat array of seatId, expiresAt, lockValue triples.

-- Lazy cleanup: drop entries whose expiry has passed. Redis expires the lock
-- keys on its own, but sorted set members have no individual TTL, so without
-- this sweep the reserved set would grow without bound and report seats as
-- held long after their locks had gone.
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])

local members = redis.call('ZRANGEBYSCORE', KEYS[1], '(' .. ARGV[1], '+inf', 'WITHSCORES')

local result = {}
for i = 1, #members, 2 do
    local seatId = members[i]
    local expiresAt = members[i + 1]
    -- The lock key carries the owner. Reading it here keeps the seat map to
    -- a single round trip instead of a follow-up MGET.
    local value = redis.call('GET', ARGV[2] .. seatId)

    -- A member with no surviving lock key is a leftover from a release that
    -- could not claim it, so skip it rather than reporting a phantom hold.
    if value then
        result[#result + 1] = seatId
        result[#result + 1] = expiresAt
        result[#result + 1] = value
    end
end

return result
