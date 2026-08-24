"use client";

import Link from "next/link";
import { useEffect, useState } from "react";

import { ApiError, listEvents } from "@/lib/api";
import { formatEventDate, formatMoney } from "@/lib/format";
import type { EventSummary } from "@/lib/types";

export default function EventsPage() {
  const [events, setEvents] = useState<EventSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();

    listEvents(controller.signal)
      .then(setEvents)
      .catch((err) => {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError(err instanceof ApiError ? err.message : "Could not load events.");
      });

    return () => controller.abort();
  }, []);

  if (error) {
    return (
      <div className="card px-6 py-12 text-center">
        <p className="text-sm text-red-300">{error}</p>
        <p className="mt-2 text-xs text-ink-500">Is the API running on port 8080?</p>
      </div>
    );
  }

  if (!events) {
    return (
      <div className="grid gap-4 sm:grid-cols-2">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="card h-44 animate-pulse" />
        ))}
      </div>
    );
  }

  return (
    <div>
      <header className="mb-8">
        <h1 className="text-2xl font-semibold tracking-tight text-ink-200">Upcoming events</h1>
        <p className="mt-1.5 text-sm text-ink-400">
          Pick an event to see its live seat map.
        </p>
      </header>

      {events.length === 0 ? (
        <div className="card px-6 py-12 text-center">
          <p className="text-sm text-ink-400">No events yet. Run the seed script to load some.</p>
          <p className="mt-2 font-mono text-xs text-ink-500">make seed</p>
        </div>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2">
          {events.map((event) => {
            const remaining = event.seats_total - event.seats_confirmed;
            const soldOut = remaining === 0;

            return (
              <Link
                key={event.id}
                href={`/events/${event.id}`}
                className="card group flex flex-col p-5 transition-colors hover:border-ink-600"
              >
                <div className="flex items-start justify-between gap-4">
                  <h2 className="text-lg font-medium text-ink-200 group-hover:text-white">
                    {event.title}
                  </h2>
                  <span className="shrink-0 rounded bg-ink-800 px-2 py-1 text-xs text-ink-300">
                    from {formatMoney(event.base_price)}
                  </span>
                </div>

                <p className="mt-2 line-clamp-2 text-sm text-ink-400">{event.description}</p>

                <dl className="mt-4 flex flex-wrap gap-x-6 gap-y-1 text-xs text-ink-400">
                  <div>
                    <dt className="inline text-ink-500">When </dt>
                    <dd className="inline">{formatEventDate(event.starts_at)}</dd>
                  </div>
                  <div>
                    <dt className="inline text-ink-500">Where </dt>
                    <dd className="inline">
                      {event.venue.name}, {event.venue.city}
                    </dd>
                  </div>
                </dl>

                <div className="mt-4 flex items-center gap-2 border-t border-ink-800 pt-3 text-xs">
                  <span
                    className={`h-1.5 w-1.5 rounded-full ${soldOut ? "bg-seat-taken" : "bg-emerald-500"}`}
                    aria-hidden
                  />
                  <span className={soldOut ? "text-seat-taken" : "text-ink-300"}>
                    {soldOut ? "Sold out" : `${remaining} of ${event.seats_total} seats available`}
                  </span>
                </div>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}
