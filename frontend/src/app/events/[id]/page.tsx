"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useState } from "react";

import { CheckoutDrawer } from "@/components/CheckoutDrawer";
import { SeatMap, SeatMapLegend } from "@/components/SeatMap";
import { ApiError, createHold, getEvent, getSeatMap } from "@/lib/api";
import { useAuth } from "@/lib/auth-context";
import { formatEventDate, formatMoney } from "@/lib/format";
import type { EventSummary, HoldResult, PayResult, SeatMapEntry, SeatUpdate } from "@/lib/types";
import { useRealtimeSeats } from "@/lib/useRealtimeSeats";

/** Matches MAX_SEATS_PER_HOLD on the server. */
const MAX_SEATS = 6;

export default function EventDetailPage() {
  const params = useParams<{ id: string }>();
  const eventId = params.id;
  const router = useRouter();
  const { user, loading: authLoading } = useAuth();

  const [event, setEvent] = useState<EventSummary | null>(null);
  const [seats, setSeats] = useState<SeatMapEntry[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [hold, setHold] = useState<HoldResult | null>(null);
  const [confirmation, setConfirmation] = useState<PayResult | null>(null);
  const [holding, setHolding] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  // --- data loading -------------------------------------------------------

  const refreshSeatMap = useCallback(async () => {
    try {
      const map = await getSeatMap(eventId);
      setSeats(map.seats);
    } catch (err) {
      if (err instanceof ApiError) setLoadError(err.message);
    }
  }, [eventId]);

  useEffect(() => {
    const controller = new AbortController();

    getEvent(eventId, controller.signal)
      .then(setEvent)
      .catch((err) => {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setLoadError(err instanceof ApiError ? err.message : "Could not load this event.");
      });

    return () => controller.abort();
  }, [eventId]);

  // --- realtime -----------------------------------------------------------

  const handleSeatUpdates = useCallback((updates: SeatUpdate[]) => {
    setSeats((current) => {
      const byId = new Map(updates.map((u) => [u.seat_id, u.status]));
      let changed = false;

      const next = current.map((seat) => {
        const status = byId.get(seat.seat_id);
        if (!status || status === seat.status) return seat;
        changed = true;
        // The broadcast carries no identity, so a seat that becomes held
        // elsewhere is not ours. Our own holds are re-established by the
        // snapshot fetch that follows every reconnect.
        return { ...seat, status, held_by_me: status === "held" ? false : seat.held_by_me };
      });

      return changed ? next : current;
    });

    // Drop any selection that somebody else has just taken.
    setSelected((current) => {
      if (current.size === 0) return current;
      const lost = updates.filter((u) => u.status !== "available" && current.has(u.seat_id));
      if (lost.length === 0) return current;

      const next = new Set(current);
      for (const update of lost) next.delete(update.seat_id);
      return next;
    });
  }, []);

  // The snapshot is fetched only once the socket is open, so no update can
  // slip through the gap between reading and subscribing.
  const connection = useRealtimeSeats({
    eventId,
    onReady: refreshSeatMap,
    onSeatUpdates: handleSeatUpdates,
  });

  // --- selection ----------------------------------------------------------

  const toggleSeat = useCallback((seat: SeatMapEntry) => {
    setError(null);
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(seat.seat_id)) {
        next.delete(seat.seat_id);
      } else {
        if (next.size >= MAX_SEATS) return current;
        next.add(seat.seat_id);
      }
      return next;
    });
  }, []);

  const selectedSeats = useMemo(
    () => seats.filter((seat) => selected.has(seat.seat_id)),
    [seats, selected],
  );

  const selectedTotal = useMemo(
    () => selectedSeats.reduce((sum, seat) => sum + seat.price, 0),
    [selectedSeats],
  );

  const atLimit = selected.size >= MAX_SEATS;

  // --- actions ------------------------------------------------------------

  async function handleHold() {
    if (!user) {
      router.push(`/login?next=${encodeURIComponent(`/events/${eventId}`)}`);
      return;
    }

    setHolding(true);
    setError(null);

    try {
      const result = await createHold(eventId, [...selected]);
      setHold(result);
      setSelected(new Set());
      await refreshSeatMap();
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message);
        // Deselect whatever the server said was gone, so the retry is clean.
        const unavailable = err.unavailableSeatIds;
        if (unavailable.length > 0) {
          setSelected((current) => {
            const next = new Set(current);
            for (const id of unavailable) next.delete(id);
            return next;
          });
        }
        await refreshSeatMap();
      } else {
        setError("Could not hold those seats. Please try again.");
      }
    } finally {
      setHolding(false);
    }
  }

  const clearHold = useCallback(async () => {
    setHold(null);
    await refreshSeatMap();
  }, [refreshSeatMap]);

  // --- render -------------------------------------------------------------

  if (loadError) {
    return (
      <div className="card px-6 py-12 text-center">
        <p className="text-sm text-red-300">{loadError}</p>
        <Link href="/events" className="btn-secondary mt-4">
          Back to events
        </Link>
      </div>
    );
  }

  if (confirmation) {
    return <Confirmation result={confirmation} eventTitle={event?.title ?? "your event"} />;
  }

  return (
    <div>
      <Link href="/events" className="text-sm text-ink-500 hover:text-ink-300">
        ← All events
      </Link>

      <header className="mt-3 flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight text-ink-200">
            {event?.title ?? "Loading…"}
          </h1>
          {event && (
            <p className="mt-1.5 text-sm text-ink-400">
              {formatEventDate(event.starts_at)} · {event.venue.name}, {event.venue.city}
            </p>
          )}
        </div>
        <ConnectionBadge state={connection} />
      </header>

      <div className="mt-8 grid gap-8 lg:grid-cols-[1fr_320px]">
        <section className="card p-5">
          <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
            <SeatMapLegend />
            <span className="text-xs text-ink-500">
              {seats.filter((s) => s.status === "available").length} available
            </span>
          </div>

          {seats.length === 0 ? (
            <div className="grid h-96 place-items-center">
              <div className="h-8 w-8 animate-spin rounded-full border-2 border-ink-700 border-t-accent" />
            </div>
          ) : (
            <SeatMap
              seats={seats}
              selectedSeatIds={selected}
              onToggleSeat={toggleSeat}
              disabled={hold !== null}
            />
          )}
        </section>

        <div>
          {hold ? (
            <CheckoutDrawer
              hold={hold}
              onReleased={clearHold}
              onExpired={() => void refreshSeatMap()}
              onConfirmed={setConfirmation}
            />
          ) : (
            <aside className="card sticky top-6 p-5">
              <h2 className="text-sm font-semibold uppercase tracking-wide text-ink-400">
                Your selection
              </h2>

              {selectedSeats.length === 0 ? (
                <p className="mt-4 text-sm text-ink-500">
                  Choose up to {MAX_SEATS} seats from the map.
                </p>
              ) : (
                <>
                  <ul className="mt-4 divide-y divide-ink-800 border-y border-ink-800">
                    {selectedSeats.map((seat) => (
                      <li key={seat.seat_id} className="flex items-center justify-between py-2.5 text-sm">
                        <span className="font-mono text-ink-300">
                          {seat.section}
                          {seat.row}-{seat.number}
                        </span>
                        <span className="text-ink-400">{formatMoney(seat.price)}</span>
                      </li>
                    ))}
                  </ul>

                  <div className="mt-4 flex items-baseline justify-between">
                    <span className="text-sm text-ink-400">Total</span>
                    <span className="text-xl font-semibold text-ink-200">{formatMoney(selectedTotal)}</span>
                  </div>
                </>
              )}

              {atLimit && (
                <p className="mt-3 text-xs text-amber-400">Maximum of {MAX_SEATS} seats per booking.</p>
              )}

              {error && (
                <p role="alert" className="mt-4 rounded-md border border-red-900/60 bg-red-950/40 px-3 py-2 text-sm text-red-300">
                  {error}
                </p>
              )}

              <button
                type="button"
                className="btn-primary mt-5 w-full"
                onClick={handleHold}
                disabled={selectedSeats.length === 0 || holding || authLoading}
              >
                {holding ? "Holding…" : user ? "Hold seats for 5 minutes" : "Sign in to hold seats"}
              </button>

              <p className="mt-3 text-center text-xs text-ink-600">
                Holding reserves your seats while you pay.
              </p>
            </aside>
          )}
        </div>
      </div>
    </div>
  );
}

function ConnectionBadge({ state }: { state: "connecting" | "live" | "reconnecting" | "offline" }) {
  const config = {
    connecting: { dot: "bg-ink-500", label: "Connecting", tone: "text-ink-500" },
    live: { dot: "bg-emerald-500", label: "Live", tone: "text-emerald-400" },
    reconnecting: { dot: "bg-amber-500 animate-pulse", label: "Reconnecting", tone: "text-amber-400" },
    offline: { dot: "bg-red-500", label: "Offline", tone: "text-red-400" },
  }[state];

  return (
    <span className="flex items-center gap-2 rounded-full border border-ink-800 bg-ink-900 px-3 py-1.5">
      <span className={`h-1.5 w-1.5 rounded-full ${config.dot}`} aria-hidden />
      <span className={`text-xs ${config.tone}`}>{config.label}</span>
    </span>
  );
}

function Confirmation({ result, eventTitle }: { result: PayResult; eventTitle: string }) {
  return (
    <div className="mx-auto max-w-md py-12 text-center">
      <div className="mx-auto grid h-12 w-12 place-items-center rounded-full bg-emerald-500/15">
        <svg viewBox="0 0 20 20" className="h-6 w-6 fill-emerald-400" aria-hidden>
          <path d="M8.1 13.5 4.6 10l-1.2 1.2 4.7 4.7 9-9-1.2-1.2z" />
        </svg>
      </div>

      <h1 className="mt-5 text-2xl font-semibold tracking-tight text-ink-200">Seats confirmed</h1>
      <p className="mt-2 text-sm text-ink-400">
        Your seats for {eventTitle} are yours. They cannot be sold to anyone else.
      </p>

      <dl className="card mt-6 divide-y divide-ink-800 text-left text-sm">
        <div className="flex justify-between px-4 py-3">
          <dt className="text-ink-500">Amount paid</dt>
          <dd className="text-ink-200">{formatMoney(result.total_amount)}</dd>
        </div>
        <div className="flex justify-between px-4 py-3">
          <dt className="text-ink-500">Reference</dt>
          <dd className="font-mono text-xs text-ink-300">{result.payment.provider_ref}</dd>
        </div>
        <div className="flex justify-between px-4 py-3">
          <dt className="text-ink-500">Booking</dt>
          <dd className="font-mono text-xs text-ink-300">{result.booking_id.slice(0, 8)}</dd>
        </div>
      </dl>

      <div className="mt-6 flex justify-center gap-2">
        <Link href="/me/bookings" className="btn-primary">
          View my bookings
        </Link>
        <Link href="/events" className="btn-secondary">
          Browse events
        </Link>
      </div>
    </div>
  );
}
