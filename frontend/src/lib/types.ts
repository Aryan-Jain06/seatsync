// Wire types, mirroring the Go models the API returns.

export type Role = "user" | "admin";

export type SeatStatus = "available" | "held" | "confirmed";

export type BookingStatus = "pending" | "confirmed" | "cancelled" | "expired";

export type PaymentStatus = "succeeded" | "failed";

export interface User {
  id: string;
  email: string;
  name: string;
  role: Role;
  created_at: string;
}

export interface Venue {
  id: string;
  name: string;
  city: string;
}

export interface EventSummary {
  id: string;
  venue_id: string;
  title: string;
  description: string;
  starts_at: string;
  /** Minor units. */
  base_price: number;
  created_at: string;
  venue: Venue;
  seats_total: number;
  seats_confirmed: number;
}

export interface SeatMapEntry {
  seat_id: string;
  section: string;
  row: number;
  number: number;
  /** Minor units. */
  price: number;
  status: SeatStatus;
  held_by_me: boolean;
}

export interface SeatMap {
  event_id: string;
  seats: SeatMapEntry[];
  available: number;
  held: number;
  confirmed: number;
}

export interface BookedSeat {
  seat_id: string;
  section: string;
  row: number;
  number: number;
  price: number;
}

export interface HoldResult {
  booking_id: string;
  event_id: string;
  expires_at: string;
  total_amount: number;
  seats: BookedSeat[];
}

export interface BookingDetail {
  id: string;
  user_id: string;
  event_id: string;
  status: BookingStatus;
  total_amount: number;
  hold_expires_at: string;
  created_at: string;
  confirmed_at?: string;
  event_title: string;
  event_starts_at: string;
  venue_name: string;
  seats: BookedSeat[];
}

export interface PaymentSummary {
  id: string;
  status: PaymentStatus;
  amount: number;
  provider_ref: string;
  created_at: string;
}

export interface PayResult {
  booking_id: string;
  status: BookingStatus;
  payment: PaymentSummary;
  total_amount: number;
  confirmed_at?: string;
  replayed: boolean;
}

export interface TokenPair {
  access_token: string;
  refresh_token: string;
  expires_at: string;
  token_type: string;
}

export interface AuthResult {
  user: User;
  tokens: TokenPair;
}

/** Seat updates pushed over the WebSocket. */
export interface SeatUpdate {
  seat_id: string;
  status: SeatStatus;
}

export interface RealtimeMessage {
  type: "connected" | "seat_update";
  event_id: string;
  seats?: SeatUpdate[];
}
