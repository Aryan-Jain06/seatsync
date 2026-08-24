"use client";

// Subscribes to an event's seat updates and keeps a seat map in step.

import { useCallback, useEffect, useRef, useState } from "react";

import { WS_BASE_URL } from "./api";
import type { RealtimeMessage, SeatStatus, SeatUpdate } from "./types";

export type ConnectionState = "connecting" | "live" | "reconnecting" | "offline";

interface Options {
  eventId: string;
  /** Called once the socket is ready, and again after each reconnect. */
  onReady: () => void;
  /** Called for each batch of seat changes. */
  onSeatUpdates: (updates: SeatUpdate[]) => void;
}

/** Backoff schedule, in milliseconds, for successive reconnect attempts. */
const BACKOFF_MS = [500, 1000, 2000, 4000, 8000, 15000];

/**
 * Opens a WebSocket for an event and reconnects when it drops.
 *
 * On every (re)connection the caller re-fetches the seat map through onReady,
 * because updates that occurred while the socket was down were missed and no
 * amount of replaying the stream would recover them.
 */
export function useRealtimeSeats({ eventId, onReady, onSeatUpdates }: Options): ConnectionState {
  const [state, setState] = useState<ConnectionState>("connecting");

  // Held in refs so a changing callback identity does not tear the socket
  // down and rebuild it on every render.
  const onReadyRef = useRef(onReady);
  const onSeatUpdatesRef = useRef(onSeatUpdates);
  useEffect(() => {
    onReadyRef.current = onReady;
    onSeatUpdatesRef.current = onSeatUpdates;
  }, [onReady, onSeatUpdates]);

  useEffect(() => {
    if (!eventId) return;

    let socket: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let attempt = 0;
    // Set when the effect tears down, so a socket that closes during unmount
    // does not schedule a reconnect for a page the user has already left.
    let disposed = false;

    const connect = (): void => {
      if (disposed) return;

      setState(attempt === 0 ? "connecting" : "reconnecting");

      try {
        socket = new WebSocket(`${WS_BASE_URL}/ws/events/${eventId}`);
      } catch {
        scheduleReconnect();
        return;
      }

      socket.onmessage = (raw: MessageEvent<string>) => {
        let message: RealtimeMessage;
        try {
          message = JSON.parse(raw.data) as RealtimeMessage;
        } catch {
          return;
        }

        if (message.type === "connected") {
          attempt = 0;
          setState("live");
          // Fetch the snapshot only now, so it cannot predate the stream.
          onReadyRef.current();
          return;
        }

        if (message.type === "seat_update" && message.seats?.length) {
          onSeatUpdatesRef.current(message.seats);
        }
      };

      socket.onerror = () => {
        // onclose always follows, which is where reconnection is handled.
      };

      socket.onclose = () => {
        socket = null;
        if (disposed) return;
        setState("reconnecting");
        scheduleReconnect();
      };
    };

    const scheduleReconnect = (): void => {
      if (disposed) return;

      const delay = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)] ?? 15000;
      attempt += 1;
      if (attempt > 3) setState("offline");

      reconnectTimer = setTimeout(connect, delay);
    };

    connect();

    return () => {
      disposed = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (socket) {
        // Detach first: the handler would otherwise queue a reconnect for a
        // component that no longer exists.
        socket.onclose = null;
        socket.close();
      }
    };
  }, [eventId]);

  return state;
}

/** Applies seat updates to a status map. */
export function applySeatUpdates(
  current: Map<string, SeatStatus>,
  updates: SeatUpdate[],
): Map<string, SeatStatus> {
  const next = new Map(current);
  for (const update of updates) next.set(update.seat_id, update.status);
  return next;
}

/** Stable callback helper, so effects are not restarted by render churn. */
export function useEventCallback<Args extends unknown[], Result>(
  fn: (...args: Args) => Result,
): (...args: Args) => Result {
  const ref = useRef(fn);
  useEffect(() => {
    ref.current = fn;
  });
  return useCallback((...args: Args) => ref.current(...args), []);
}
