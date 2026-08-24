-- Acquire locks over a set of seats, all or nothing.
--
-- The whole script runs atomically inside Redis, so no other client can
-- observe or interleave with the gap between the availability check and the
-- writes. That is what makes "either every requested seat is locked for this
-- caller, or none are" true under concurrency: two callers racing for an
-- overlapping set cannot both pass the check.
--
-- KEYS[i]      lock key for seat i, i.e. lock:<eventId>:<seatId>
-- ARGV[1]      lock value, "<userId>:<lockToken>"
-- ARGV[2]      lock time-to-live, in seconds
-- ARGV[3]      reserved-set key for the event, event:<eventId>:reserved
-- ARGV[4]      hold expiry as a unix timestamp in milliseconds
-- ARGV[4 + i]  seat id for KEYS[i], returned when unavailable and used as the
--              member in the reserved set
--
-- Returns an array of the seat ids that were already locked. An empty array
-- means every requested seat was acquired.

local unavailable = {}

for i = 1, #KEYS do
    if redis.call('EXISTS', KEYS[i]) == 1 then
        unavailable[#unavailable + 1] = ARGV[4 + i]
    end
end

-- Nothing is written when any seat is taken, so a failed attempt leaves no
-- trace and cannot partially block a competing caller.
if #unavailable > 0 then
    return unavailable
end

local ttl = tonumber(ARGV[2])
local reservedKey = ARGV[3]
local expiresAt = ARGV[4]

for i = 1, #KEYS do
    -- NX is redundant given the EXISTS sweep above, but it keeps the write
    -- itself conditional: if this script's assumptions were ever wrong, the
    -- SET fails closed rather than stealing a live lock.
    if not redis.call('SET', KEYS[i], ARGV[1], 'NX', 'EX', ttl) then
        return redis.error_reply('lock contention on ' .. ARGV[4 + i])
    end
    -- The sorted set is the read model: it lets the seat map find every held
    -- seat for an event in one query, which scanning lock keys could not do
    -- without a KEYS/SCAN sweep across the keyspace.
    redis.call('ZADD', reservedKey, expiresAt, ARGV[4 + i])
end

-- Keep the reserved set from outliving the locks it describes.
redis.call('EXPIRE', reservedKey, ttl + 60)

return {}
