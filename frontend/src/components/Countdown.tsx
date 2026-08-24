"use client";

import { useEffect, useState } from "react";

import { formatCountdown } from "@/lib/format";

interface Props {
  /** ISO timestamp the hold lapses at. */
  expiresAt: string;
  onExpired: () => void;
}

/**
 * Counts down to a hold's expiry.
 *
 * The remaining time is recomputed from the target timestamp on every tick
 * rather than decremented, so a throttled background tab or a sleeping
 * machine shows the true remaining time when it wakes rather than a stale
 * count that drifted while the timer was not firing.
 */
export function Countdown({ expiresAt, onExpired }: Props) {
  const target = new Date(expiresAt).getTime();
  const [remaining, setRemaining] = useState(() => target - Date.now());

  useEffect(() => {
    setRemaining(target - Date.now());

    const timer = setInterval(() => {
      const next = target - Date.now();
      setRemaining(next);
      if (next <= 0) {
        clearInterval(timer);
        onExpired();
      }
    }, 250);

    return () => clearInterval(timer);
    // onExpired is intentionally excluded: callers pass an inline function,
    // and depending on it would restart the timer on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target]);

  const expired = remaining <= 0;
  // Below a minute the countdown turns amber, then red in the last 30s.
  const tone = expired || remaining < 30_000 ? "text-red-400" : remaining < 60_000 ? "text-amber-400" : "text-ink-200";

  return (
    <div className="flex items-baseline gap-2">
      <span className="text-xs uppercase tracking-wide text-ink-500">
        {expired ? "Hold expired" : "Seats held for"}
      </span>
      <span className={`font-mono text-lg font-semibold tabular-nums ${tone}`} role="timer" aria-live="off">
        {formatCountdown(remaining)}
      </span>
    </div>
  );
}
