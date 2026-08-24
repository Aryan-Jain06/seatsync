-- Release locks previously taken by this caller.
--
-- A lock is only deleted when its stored value still matches the caller's
-- "<userId>:<lockToken>". Without that check a slow release could delete a
-- lock that had already expired and been re-acquired by somebody else,
-- handing that person's seat to a third party. Check-then-delete would be
-- racy as two round trips; inside this script it is atomic.
--
-- KEYS[i]      lock key for seat i
-- ARGV[1]      expected lock value, "<userId>:<lockToken>"
-- ARGV[2]      reserved-set key for the event
-- ARGV[2 + i]  seat id for KEYS[i]
--
-- Returns the number of locks actually released.

local released = 0

for i = 1, #KEYS do
    if redis.call('GET', KEYS[i]) == ARGV[1] then
        redis.call('DEL', KEYS[i])
        -- Only drop the reserved-set entry alongside a lock that was
        -- provably ours. If the lock had already lapsed and been retaken,
        -- the entry now describes the new holder's claim, and removing it
        -- would wrongly advertise their seat as free. Genuinely stale
        -- entries are swept by score when the seat map is read.
        redis.call('ZREM', ARGV[2], ARGV[2 + i])
        released = released + 1
    end
end

return released
