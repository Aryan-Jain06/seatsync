"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";

import { ApiError, listBookings } from "@/lib/api";
import { useAuth } from "@/lib/auth-context";
import { formatEventDate, formatMoney, formatShortDate, seatLabel } from "@/lib/format";
import type { BookingDetail, BookingStatus } from "@/lib/types";

const STATUS_STYLES: Record<BookingStatus, { label: string; className: string }> = {
  confirmed: { label: "Confirmed", className: "border-emerald-900/60 bg-emerald-950/40 text-emerald-300" },
  pending: { label: "Awaiting payment", className: "border-amber-900/60 bg-amber-950/40 text-amber-300" },
  cancelled: { label: "Released", className: "border-ink-700 bg-ink-850 text-ink-400" },
  expired: { label: "Hold expired", className: "border-ink-700 bg-ink-850 text-ink-400" },
};

export default function BookingsPage() {
  const { user, loading: authLoading } = useAuth();
  const router = useRouter();

  const [bookings, setBookings] = useState<BookingDetail[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // Wait for the session restore to settle before deciding the user is
    // signed out, otherwise a reload bounces them to the login page.
    if (authLoading) return;

    if (!user) {
      router.replace("/login?next=%2Fme%2Fbookings");
      return;
    }

    const controller = new AbortController();

    listBookings(controller.signal)
      .then(setBookings)
      .catch((err) => {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError(err instanceof ApiError ? err.message : "Could not load your bookings.");
      });

    return () => controller.abort();
  }, [authLoading, user, router]);

  if (authLoading || (!user && !error)) {
    return <div className="card h-64 animate-pulse" />;
  }

  return (
    <div>
      <header className="mb-8">
        <h1 className="text-2xl font-semibold tracking-tight text-ink-200">My bookings</h1>
        <p className="mt-1.5 text-sm text-ink-400">Every hold and purchase on your account.</p>
      </header>

      {error && (
        <div className="card px-6 py-12 text-center">
          <p className="text-sm text-red-300">{error}</p>
        </div>
      )}

      {!error && !bookings && (
        <div className="space-y-4">
          {[0, 1].map((i) => (
            <div key={i} className="card h-32 animate-pulse" />
          ))}
        </div>
      )}

      {bookings?.length === 0 && (
        <div className="card px-6 py-12 text-center">
          <p className="text-sm text-ink-400">You have not booked anything yet.</p>
          <Link href="/events" className="btn-primary mt-4">
            Browse events
          </Link>
        </div>
      )}

      <div className="space-y-4">
        {bookings?.map((booking) => {
          const status = STATUS_STYLES[booking.status];

          return (
            <article key={booking.id} className="card p-5">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <h2 className="font-medium text-ink-200">{booking.event_title}</h2>
                  <p className="mt-1 text-sm text-ink-400">
                    {formatEventDate(booking.event_starts_at)} · {booking.venue_name}
                  </p>
                </div>
                <span className={`rounded-full border px-2.5 py-1 text-xs ${status.className}`}>
                  {status.label}
                </span>
              </div>

              <div className="mt-4 flex flex-wrap items-center gap-1.5">
                {booking.seats.map((seat) => (
                  <span
                    key={seat.seat_id}
                    className="rounded border border-ink-700 bg-ink-850 px-2 py-1 font-mono text-xs text-ink-300"
                  >
                    {seatLabel(seat)}
                  </span>
                ))}
              </div>

              <div className="mt-4 flex flex-wrap items-baseline justify-between gap-2 border-t border-ink-800 pt-3">
                <span className="text-xs text-ink-500">
                  Booked {formatShortDate(booking.created_at)}
                  {booking.confirmed_at && ` · paid ${formatShortDate(booking.confirmed_at)}`}
                </span>
                <span className="font-medium text-ink-200">{formatMoney(booking.total_amount)}</span>
              </div>

              {booking.status === "pending" && (
                <Link href={`/events/${booking.event_id}`} className="btn-secondary mt-4 w-full">
                  Return to this event
                </Link>
              )}
            </article>
          );
        })}
      </div>
    </div>
  );
}
