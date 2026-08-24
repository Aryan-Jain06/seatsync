"use client";

import { useMemo } from "react";

import { formatMoney, seatLabel } from "@/lib/format";
import type { SeatMapEntry } from "@/lib/types";

/** Geometry of the rendered map, in SVG user units. */
const SEAT_SIZE = 22;
const SEAT_GAP = 5;
const ROW_GAP = 5;
const SECTION_GAP = 34;
const ROW_LABEL_WIDTH = 26;
const STAGE_HEIGHT = 34;
const STAGE_MARGIN = 26;

/** How a seat should be drawn. */
type SeatVisual = "available" | "selected" | "mine" | "taken" | "sold";

const SEAT_FILL: Record<SeatVisual, string> = {
  available: "#3f4756",
  selected: "#f5c542",
  mine: "#f5c542",
  taken: "#c0392b",
  sold: "#1b1f28",
};

const SEAT_STROKE: Record<SeatVisual, string> = {
  available: "#5b6577",
  selected: "#fbe08a",
  mine: "#fbe08a",
  taken: "#e05a4a",
  sold: "#2a303c",
};

/**
 * Decides how one seat is drawn.
 *
 * Selection is checked before anything else so a click registers instantly,
 * without waiting for the hold request to return.
 */
function visualFor(seat: SeatMapEntry, isSelected: boolean): SeatVisual {
  if (seat.status === "confirmed") return "sold";
  if (isSelected) return "selected";
  if (seat.status === "held") return seat.held_by_me ? "mine" : "taken";
  return "available";
}

function isSelectable(seat: SeatMapEntry): boolean {
  return seat.status === "available";
}

interface Props {
  seats: SeatMapEntry[];
  selectedSeatIds: Set<string>;
  onToggleSeat: (seat: SeatMapEntry) => void;
  /** Disables interaction while a hold is in flight or already placed. */
  disabled?: boolean;
}

export function SeatMap({ seats, selectedSeatIds, onToggleSeat, disabled = false }: Props) {
  // Grouping is derived from the seat list rather than assumed, so the map
  // renders whatever shape the venue actually has.
  const layout = useMemo(() => {
    const sections = new Map<string, Map<number, SeatMapEntry[]>>();

    for (const seat of seats) {
      let rows = sections.get(seat.section);
      if (!rows) {
        rows = new Map<number, SeatMapEntry[]>();
        sections.set(seat.section, rows);
      }
      const row = rows.get(seat.row);
      if (row) row.push(seat);
      else rows.set(seat.row, [seat]);
    }

    const ordered = [...sections.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([name, rowMap]) => ({
        name,
        rows: [...rowMap.entries()]
          .sort(([a], [b]) => a - b)
          .map(([number, rowSeats]) => ({
            number,
            seats: [...rowSeats].sort((a, b) => a.number - b.number),
          })),
      }));

    const widestRow = Math.max(1, ...ordered.flatMap((s) => s.rows.map((r) => r.seats.length)));
    const width = ROW_LABEL_WIDTH * 2 + widestRow * (SEAT_SIZE + SEAT_GAP) - SEAT_GAP;

    let height = STAGE_HEIGHT + STAGE_MARGIN;
    const sectionOffsets: number[] = [];
    for (const section of ordered) {
      sectionOffsets.push(height);
      height += 20; // section heading
      height += section.rows.length * (SEAT_SIZE + ROW_GAP);
      height += SECTION_GAP;
    }

    return { sections: ordered, width, height, sectionOffsets };
  }, [seats]);

  if (seats.length === 0) {
    return <div className="card grid h-96 place-items-center text-sm text-ink-500">No seats to show.</div>;
  }

  return (
    <svg
      viewBox={`0 0 ${layout.width} ${layout.height}`}
      className="w-full select-none"
      role="group"
      aria-label="Seat map"
    >
      {/* Stage, so the seat numbering has an orientation. */}
      <rect
        x={ROW_LABEL_WIDTH}
        y={0}
        width={layout.width - ROW_LABEL_WIDTH * 2}
        height={STAGE_HEIGHT}
        rx={4}
        fill="#12151c"
        stroke="#232834"
      />
      <text
        x={layout.width / 2}
        y={STAGE_HEIGHT / 2 + 4}
        textAnchor="middle"
        className="fill-ink-500"
        style={{ fontSize: 11, letterSpacing: "0.18em" }}
      >
        STAGE
      </text>

      {layout.sections.map((section, sectionIndex) => {
        const sectionTop = layout.sectionOffsets[sectionIndex] ?? 0;

        return (
          <g key={section.name}>
            <text
              x={0}
              y={sectionTop + 12}
              className="fill-ink-400"
              style={{ fontSize: 11, fontWeight: 600, letterSpacing: "0.08em" }}
            >
              SECTION {section.name}
            </text>

            {section.rows.map((row, rowIndex) => {
              const y = sectionTop + 20 + rowIndex * (SEAT_SIZE + ROW_GAP);
              const rowWidth = row.seats.length * (SEAT_SIZE + SEAT_GAP) - SEAT_GAP;
              const startX = (layout.width - rowWidth) / 2;

              return (
                <g key={row.number}>
                  <text
                    x={startX - 9}
                    y={y + SEAT_SIZE / 2 + 4}
                    textAnchor="end"
                    className="fill-ink-600"
                    style={{ fontSize: 10 }}
                  >
                    {row.number}
                  </text>

                  {row.seats.map((seat, seatIndex) => {
                    const selected = selectedSeatIds.has(seat.seat_id);
                    const visual = visualFor(seat, selected);
                    const selectable = isSelectable(seat) && !disabled;
                    const x = startX + seatIndex * (SEAT_SIZE + SEAT_GAP);

                    return (
                      <rect
                        key={seat.seat_id}
                        x={x}
                        y={y}
                        width={SEAT_SIZE}
                        height={SEAT_SIZE}
                        rx={3}
                        fill={SEAT_FILL[visual]}
                        stroke={SEAT_STROKE[visual]}
                        strokeWidth={selected ? 2 : 1}
                        className={selectable ? "cursor-pointer transition-[fill] hover:brightness-125" : "cursor-not-allowed"}
                        role="button"
                        tabIndex={selectable ? 0 : -1}
                        aria-label={`Seat ${seatLabel(seat)}, ${formatMoney(seat.price)}, ${describe(seat, selected)}`}
                        aria-pressed={selected}
                        aria-disabled={!selectable}
                        onClick={selectable ? () => onToggleSeat(seat) : undefined}
                        onKeyDown={
                          selectable
                            ? (event) => {
                                if (event.key === "Enter" || event.key === " ") {
                                  event.preventDefault();
                                  onToggleSeat(seat);
                                }
                              }
                            : undefined
                        }
                      >
                        <title>
                          {seatLabel(seat)} · {formatMoney(seat.price)} · {describe(seat, selected)}
                        </title>
                      </rect>
                    );
                  })}
                </g>
              );
            })}
          </g>
        );
      })}
    </svg>
  );
}

function describe(seat: SeatMapEntry, selected: boolean): string {
  if (seat.status === "confirmed") return "sold";
  if (selected) return "selected";
  if (seat.status === "held") return seat.held_by_me ? "held by you" : "held by someone else";
  return "available";
}

/** The legend, sharing SEAT_FILL so it cannot drift from the map. */
export function SeatMapLegend() {
  const entries: Array<{ visual: SeatVisual; label: string }> = [
    { visual: "available", label: "Available" },
    { visual: "selected", label: "Your selection" },
    { visual: "taken", label: "Held by someone else" },
    { visual: "sold", label: "Sold" },
  ];

  return (
    <ul className="flex flex-wrap items-center gap-x-5 gap-y-2">
      {entries.map((entry) => (
        <li key={entry.visual} className="flex items-center gap-2 text-xs text-ink-400">
          <span
            className="h-3 w-3 rounded-sm border"
            style={{ backgroundColor: SEAT_FILL[entry.visual], borderColor: SEAT_STROKE[entry.visual] }}
            aria-hidden
          />
          {entry.label}
        </li>
      ))}
    </ul>
  );
}
