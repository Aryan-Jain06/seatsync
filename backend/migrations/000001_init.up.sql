-- SeatSync initial schema.
--
-- Money is stored as BIGINT in minor units (paise/cents), never as a float.
-- Integer arithmetic on minor units is exact, which matters because seat
-- prices are derived by multiplying a base price by a per-seat multiplier
-- and then summed into a booking total.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------

CREATE TYPE user_role AS ENUM ('user', 'admin');

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    role          user_role   NOT NULL DEFAULT 'user',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Emails are compared case-insensitively. Storing them lowercased would work
-- too, but a functional unique index keeps the original casing for display
-- while still rejecting "Alice@x.com" when "alice@x.com" exists.
CREATE UNIQUE INDEX uq_users_email ON users (lower(email));

-- ---------------------------------------------------------------------------
-- refresh_tokens
--
-- Refresh tokens live in the database so they can be revoked; access tokens
-- are stateless JWTs and are not stored anywhere. Only a SHA-256 hash of the
-- token is kept, so a database leak does not hand out usable sessions.
-- ---------------------------------------------------------------------------

CREATE TABLE refresh_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash BYTEA       NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_refresh_tokens_hash ON refresh_tokens (token_hash);
CREATE INDEX ix_refresh_tokens_user ON refresh_tokens (user_id);

-- ---------------------------------------------------------------------------
-- venues and seats
--
-- Seats belong to a venue, not to an event: the physical seat "A-3-7" exists
-- once and is reused by every event held at that venue. What varies per event
-- is whether the seat is booked, which lives in booking_seats.
-- ---------------------------------------------------------------------------

CREATE TABLE venues (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL,
    city       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE seats (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id         UUID          NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    section          TEXT          NOT NULL,
    "row"            INTEGER       NOT NULL,
    number           INTEGER       NOT NULL,
    -- Multiplier applied to the event's base price, e.g. 1.50 for a front row.
    price_multiplier NUMERIC(4, 2) NOT NULL DEFAULT 1.00
        CONSTRAINT ck_seats_multiplier_positive CHECK (price_multiplier > 0),

    CONSTRAINT uq_seats_position UNIQUE (venue_id, section, "row", number)
);

CREATE INDEX ix_seats_venue ON seats (venue_id);

-- ---------------------------------------------------------------------------
-- events
-- ---------------------------------------------------------------------------

CREATE TABLE events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id    UUID        NOT NULL REFERENCES venues (id) ON DELETE RESTRICT,
    title       TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    starts_at   TIMESTAMPTZ NOT NULL,
    -- Minor units. Seat price = round(base_price * seats.price_multiplier).
    base_price  BIGINT      NOT NULL
        CONSTRAINT ck_events_base_price_positive CHECK (base_price > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ix_events_venue ON events (venue_id);
CREATE INDEX ix_events_starts_at ON events (starts_at);

-- ---------------------------------------------------------------------------
-- bookings
-- ---------------------------------------------------------------------------

CREATE TYPE booking_status AS ENUM ('pending', 'confirmed', 'cancelled', 'expired');

CREATE TABLE bookings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID           NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    event_id        UUID           NOT NULL REFERENCES events (id) ON DELETE RESTRICT,
    status          booking_status NOT NULL DEFAULT 'pending',
    total_amount    BIGINT         NOT NULL
        CONSTRAINT ck_bookings_total_non_negative CHECK (total_amount >= 0),
    -- Set on the first payment attempt and thereafter immutable, so a retry
    -- carrying the same Idempotency-Key is recognised as the same attempt.
    idempotency_key TEXT,
    -- Random per-booking token. The Redis lock for each held seat stores
    -- "<user_id>:<lock_token>" as its value, so a release or a confirm can
    -- prove it owns the lock it is about to act on.
    lock_token      TEXT           NOT NULL,
    -- When the Redis holds backing this booking lapse. Used by the expiry
    -- worker; Redis TTLs remain the source of truth for the locks themselves.
    hold_expires_at TIMESTAMPTZ    NOT NULL,
    created_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),
    confirmed_at    TIMESTAMPTZ,

    -- confirmed_at is set exactly when the booking is confirmed.
    CONSTRAINT ck_bookings_confirmed_at CHECK (
        (status = 'confirmed' AND confirmed_at IS NOT NULL)
        OR (status <> 'confirmed' AND confirmed_at IS NULL)
    )
);

CREATE UNIQUE INDEX uq_bookings_idempotency_key
    ON bookings (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX ix_bookings_user ON bookings (user_id, created_at DESC);
CREATE INDEX ix_bookings_event ON bookings (event_id);

-- Drives the expiry worker's sweep: find pending bookings past their hold.
CREATE INDEX ix_bookings_pending_expiry
    ON bookings (hold_expires_at)
    WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- booking_seats
--
-- This table carries the system's hard guarantee.
--
-- `confirmed` is denormalised from bookings.status so that a *partial* unique
-- index can be declared over it. The index below means Postgres will reject
-- any attempt to record a second confirmed sale of the same seat for the same
-- event, no matter what happened upstream: expired Redis locks, a Redis
-- outage, a bug in the service layer, or two transactions racing on the exact
-- same millisecond. Redis prevents contention; this index makes the invariant
-- impossible to violate.
--
-- Rows for pending bookings are deliberately NOT covered by the index, so
-- several users may hold pending bookings over the same seat (the Redis lock
-- is what stops that in practice) without tripping a database error.
-- ---------------------------------------------------------------------------

CREATE TABLE booking_seats (
    booking_id UUID    NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    seat_id    UUID    NOT NULL REFERENCES seats (id) ON DELETE RESTRICT,
    event_id   UUID    NOT NULL REFERENCES events (id) ON DELETE RESTRICT,
    confirmed  BOOLEAN NOT NULL DEFAULT FALSE,

    PRIMARY KEY (booking_id, seat_id)
);

-- The invariant: one confirmed booking per (event, seat). Ever.
CREATE UNIQUE INDEX uq_booking_seats_confirmed_seat
    ON booking_seats (event_id, seat_id)
    WHERE confirmed;

CREATE INDEX ix_booking_seats_event ON booking_seats (event_id);
CREATE INDEX ix_booking_seats_booking ON booking_seats (booking_id);

-- ---------------------------------------------------------------------------
-- payments
-- ---------------------------------------------------------------------------

CREATE TYPE payment_status AS ENUM ('succeeded', 'failed');

CREATE TABLE payments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id   UUID           NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    status       payment_status NOT NULL,
    amount       BIGINT         NOT NULL,
    -- Reference returned by the (mock) payment provider.
    provider_ref TEXT           NOT NULL,
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT now()
);

CREATE INDEX ix_payments_booking ON payments (booking_id, created_at DESC);

-- At most one successful payment per booking. A second charge for the same
-- booking is rejected by the database even if idempotency handling upstream
-- were to fail.
CREATE UNIQUE INDEX uq_payments_one_success_per_booking
    ON payments (booking_id)
    WHERE status = 'succeeded';
