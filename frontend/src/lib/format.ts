// Presentation helpers.

/**
 * Money arrives from the API in minor units, so it is divided only at the
 * moment it is displayed. Doing arithmetic on the divided value would
 * reintroduce the rounding errors the integer representation avoids.
 */
export function formatMoney(minorUnits: number): string {
  return new Intl.NumberFormat("en-IN", {
    style: "currency",
    currency: "INR",
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(minorUnits / 100);
}

export function formatEventDate(iso: string): string {
  return new Date(iso).toLocaleString("en-IN", {
    weekday: "short",
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

export function formatShortDate(iso: string): string {
  return new Date(iso).toLocaleDateString("en-IN", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

/** Renders a seat as the label printed on a ticket. */
export function seatLabel(seat: { section: string; row: number; number: number }): string {
  return `${seat.section}${seat.row}-${seat.number}`;
}

/** Formats a remaining duration as m:ss, clamped at zero. */
export function formatCountdown(millisRemaining: number): string {
  const totalSeconds = Math.max(0, Math.floor(millisRemaining / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${seconds.toString().padStart(2, "0")}`;
}
