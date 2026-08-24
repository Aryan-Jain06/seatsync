"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, payForBooking, releaseHold } from "@/lib/api";
import { formatMoney, seatLabel } from "@/lib/format";
import type { HoldResult, PayResult } from "@/lib/types";

import { Countdown } from "./Countdown";

interface Props {
  hold: HoldResult;
  onReleased: () => void;
  onExpired: () => void;
  onConfirmed: (result: PayResult) => void;
}

export function CheckoutDrawer({ hold, onReleased, onExpired, onConfirmed }: Props) {
  const [paying, setPaying] = useState(false);
  const [releasing, setReleasing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expired, setExpired] = useState(false);

  /**
   * The idempotency key is generated once per checkout and reused by every
   * retry. Generating a fresh one on retry would present the API with what
   * looks like a second purchase, which is exactly the double charge the key
   * exists to prevent.
   */
  const idempotencyKeyRef = useRef<string>(crypto.randomUUID());

  // A new hold is a new checkout and therefore needs a new key.
  useEffect(() => {
    idempotencyKeyRef.current = crypto.randomUUID();
    setError(null);
    setExpired(false);
  }, [hold.booking_id]);

  const handleExpired = useCallback(() => {
    setExpired(true);
    onExpired();
  }, [onExpired]);

  async function handlePay() {
    setPaying(true);
    setError(null);

    try {
      const result = await payForBooking(hold.booking_id, idempotencyKeyRef.current);
      onConfirmed(result);
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message);
        if (err.code === "hold_expired") setExpired(true);
      } else {
        setError("Payment could not be completed. Please try again.");
      }
      setPaying(false);
    }
  }

  async function handleRelease() {
    setReleasing(true);
    try {
      await releaseHold(hold.booking_id);
    } catch {
      // The hold lapses on its own, so a failed release still leaves the
      // user in a sane state.
    }
    onReleased();
  }

  return (
    <aside className="card sticky top-6 flex flex-col gap-5 p-5" aria-label="Checkout">
      <div className="flex items-center justify-between gap-4">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-ink-400">Checkout</h2>
        <Countdown expiresAt={hold.expires_at} onExpired={handleExpired} />
      </div>

      <ul className="divide-y divide-ink-800 border-y border-ink-800">
        {hold.seats.map((seat) => (
          <li key={seat.seat_id} className="flex items-center justify-between py-2.5 text-sm">
            <span className="font-mono text-ink-300">{seatLabel(seat)}</span>
            <span className="text-ink-400">{formatMoney(seat.price)}</span>
          </li>
        ))}
      </ul>

      <div className="flex items-baseline justify-between">
        <span className="text-sm text-ink-400">
          Total · {hold.seats.length} seat{hold.seats.length === 1 ? "" : "s"}
        </span>
        <span className="text-xl font-semibold text-ink-200">{formatMoney(hold.total_amount)}</span>
      </div>

      {error && (
        <div role="alert" className="rounded-md border border-red-900/60 bg-red-950/40 px-3 py-2.5">
          <p className="text-sm text-red-300">{error}</p>
          {!expired && (
            <p className="mt-1 text-xs text-red-400/80">
              Your seats are still held. Retrying reuses the same payment key, so you cannot be charged twice.
            </p>
          )}
        </div>
      )}

      <div className="flex flex-col gap-2">
        <button type="button" className="btn-primary w-full" onClick={handlePay} disabled={paying || expired || releasing}>
          {paying ? "Processing payment…" : error ? "Retry payment" : `Pay ${formatMoney(hold.total_amount)}`}
        </button>

        <button type="button" className="btn-secondary w-full" onClick={handleRelease} disabled={paying || releasing}>
          {releasing ? "Releasing…" : expired ? "Start over" : "Release seats"}
        </button>
      </div>

      <p className="text-center text-xs text-ink-600">
        Payments are simulated. No card is charged.
      </p>
    </aside>
  );
}
