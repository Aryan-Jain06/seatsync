// The HTTP client. It owns the access token, refreshes it when the API says
// it has expired, and turns error responses into a typed ApiError.

import type {
  AuthResult,
  BookingDetail,
  EventSummary,
  HoldResult,
  PayResult,
  SeatMap,
  TokenPair,
  User,
} from "./types";

export const API_BASE_URL =
  process.env.NEXT_PUBLIC_API_BASE_URL ?? "http://localhost:8080";

export const WS_BASE_URL =
  process.env.NEXT_PUBLIC_WS_BASE_URL ?? "ws://localhost:8080";

/** Stable error codes the API returns. */
export type ApiErrorCode =
  | "bad_request"
  | "validation_failed"
  | "unauthorized"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "seats_unavailable"
  | "hold_expired"
  | "payment_failed"
  | "rate_limited"
  | "internal_error"
  | "network_error";

export class ApiError extends Error {
  readonly status: number;
  readonly code: ApiErrorCode;
  readonly details?: Record<string, unknown>;

  constructor(status: number, code: ApiErrorCode, message: string, details?: Record<string, unknown>) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }

  /** Seat ids the API reported as unavailable, if any. */
  get unavailableSeatIds(): string[] {
    const ids = this.details?.["unavailable_seat_ids"] ?? this.details?.["expired_seat_ids"];
    return Array.isArray(ids) ? (ids as string[]) : [];
  }

  /** True when retrying the same request could still succeed. */
  get isRetryable(): boolean {
    return this.code === "payment_failed" || this.status >= 500;
  }
}

/**
 * Tokens live in module memory rather than localStorage.
 *
 * A token in localStorage is readable by any script that ends up on the page,
 * so a single XSS becomes a stolen session that outlives the tab. Memory is
 * cleared on reload, which costs a refresh round trip on load and is the
 * trade this app makes.
 *
 * The refresh token is the exception: it is kept in sessionStorage so a page
 * reload can recover a session, and it is useless without the API.
 */
const REFRESH_STORAGE_KEY = "seatsync.refresh";

let accessToken: string | null = null;
let currentUser: User | null = null;

/** Notified whenever the signed-in user changes. */
type AuthListener = (user: User | null) => void;
const listeners = new Set<AuthListener>();

function notify(): void {
  for (const listener of listeners) listener(currentUser);
}

export function onAuthChange(listener: AuthListener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function getCurrentUser(): User | null {
  return currentUser;
}

export function getAccessToken(): string | null {
  return accessToken;
}

function readRefreshToken(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return window.sessionStorage.getItem(REFRESH_STORAGE_KEY);
  } catch {
    // Private browsing modes can throw on access.
    return null;
  }
}

function writeRefreshToken(token: string | null): void {
  if (typeof window === "undefined") return;
  try {
    if (token === null) window.sessionStorage.removeItem(REFRESH_STORAGE_KEY);
    else window.sessionStorage.setItem(REFRESH_STORAGE_KEY, token);
  } catch {
    // Not fatal: the session simply will not survive a reload.
  }
}

function applyTokens(tokens: TokenPair, user: User): void {
  accessToken = tokens.access_token;
  writeRefreshToken(tokens.refresh_token);
  currentUser = user;
  notify();
}

export function clearSession(): void {
  accessToken = null;
  currentUser = null;
  writeRefreshToken(null);
  notify();
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  headers?: Record<string, string>;
  /** Set for endpoints that work without a session. */
  anonymous?: boolean;
  signal?: AbortSignal;
}

/** Parses an error response into an ApiError. */
async function toApiError(response: Response): Promise<ApiError> {
  let code: ApiErrorCode = "internal_error";
  let message = `Request failed with status ${response.status}.`;
  let details: Record<string, unknown> | undefined;

  try {
    const body = await response.json();
    if (body?.error) {
      code = body.error.code ?? code;
      message = body.error.message ?? message;
      details = body.error.details;
    }
  } catch {
    // A non-JSON body leaves the defaults in place.
  }
  return new ApiError(response.status, code, message, details);
}

/**
 * Refresh is de-duplicated: several requests can fail on an expired token at
 * once, and each retrying independently would rotate the refresh token
 * repeatedly, which the API treats as replay and answers by ending every
 * session. Sharing one in-flight promise means one rotation.
 */
let refreshInFlight: Promise<boolean> | null = null;

async function refreshSession(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight;

  refreshInFlight = (async () => {
    const refreshToken = readRefreshToken();
    if (!refreshToken) return false;

    try {
      const response = await fetch(`${API_BASE_URL}/auth/refresh`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ refresh_token: refreshToken }),
      });

      if (!response.ok) {
        clearSession();
        return false;
      }

      const result: AuthResult = await response.json();
      applyTokens(result.tokens, result.user);
      return true;
    } catch {
      return false;
    } finally {
      refreshInFlight = null;
    }
  })();

  return refreshInFlight;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, headers = {}, anonymous = false, signal } = options;

  const send = async (): Promise<Response> => {
    const requestHeaders: Record<string, string> = { ...headers };
    if (body !== undefined) requestHeaders["Content-Type"] = "application/json";
    if (!anonymous && accessToken) requestHeaders["Authorization"] = `Bearer ${accessToken}`;

    return fetch(`${API_BASE_URL}${path}`, {
      method,
      headers: requestHeaders,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  };

  let response: Response;
  try {
    response = await send();
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw new ApiError(0, "network_error", "Could not reach the server. Check your connection and try again.");
  }

  // One retry, and only for an expired token. Retrying a 401 caused by
  // anything else would loop.
  if (response.status === 401 && !anonymous) {
    const error = await toApiError(response.clone());
    if (error.message.toLowerCase().includes("expired") && (await refreshSession())) {
      try {
        response = await send();
      } catch {
        throw new ApiError(0, "network_error", "Could not reach the server. Check your connection and try again.");
      }
    }
  }

  if (!response.ok) throw await toApiError(response);
  if (response.status === 204) return undefined as T;

  return (await response.json()) as T;
}

// --- auth -----------------------------------------------------------------

export async function register(email: string, password: string, name: string): Promise<User> {
  const result = await request<AuthResult>("/auth/register", {
    method: "POST",
    body: { email, password, name },
    anonymous: true,
  });
  applyTokens(result.tokens, result.user);
  return result.user;
}

export async function login(email: string, password: string): Promise<User> {
  const result = await request<AuthResult>("/auth/login", {
    method: "POST",
    body: { email, password },
    anonymous: true,
  });
  applyTokens(result.tokens, result.user);
  return result.user;
}

export async function logout(): Promise<void> {
  const refreshToken = readRefreshToken();
  clearSession();
  if (!refreshToken) return;
  try {
    await request<void>("/auth/logout", {
      method: "POST",
      body: { refresh_token: refreshToken },
      anonymous: true,
    });
  } catch {
    // The local session is already gone, which is what the user asked for.
  }
}

/**
 * Restores a session on page load from the stored refresh token.
 * Returns null when there is nothing to restore.
 */
export async function restoreSession(): Promise<User | null> {
  if (currentUser) return currentUser;
  if (!readRefreshToken()) return null;
  return (await refreshSession()) ? currentUser : null;
}

// --- catalogue ------------------------------------------------------------

export async function listEvents(signal?: AbortSignal): Promise<EventSummary[]> {
  const result = await request<{ events: EventSummary[] }>("/events", { anonymous: true, signal });
  return result.events;
}

export async function getEvent(eventId: string, signal?: AbortSignal): Promise<EventSummary> {
  return request<EventSummary>(`/events/${eventId}`, { anonymous: true, signal });
}

export async function getSeatMap(eventId: string, signal?: AbortSignal): Promise<SeatMap> {
  // Sent with credentials when available, so held_by_me is populated.
  return request<SeatMap>(`/events/${eventId}/seatmap`, { signal });
}

// --- holds and bookings ---------------------------------------------------

export async function createHold(eventId: string, seatIds: string[]): Promise<HoldResult> {
  return request<HoldResult>(`/events/${eventId}/holds`, {
    method: "POST",
    body: { seat_ids: seatIds },
  });
}

export async function releaseHold(bookingId: string): Promise<void> {
  return request<void>(`/holds/${bookingId}`, { method: "DELETE" });
}

export async function listBookings(signal?: AbortSignal): Promise<BookingDetail[]> {
  const result = await request<{ bookings: BookingDetail[] }>("/me/bookings", { signal });
  return result.bookings;
}

/**
 * Pays for a booking.
 *
 * The idempotency key is supplied by the caller and must be generated once
 * per checkout, then reused across retries. That is what lets the API tell a
 * retry apart from a second purchase.
 */
export async function payForBooking(bookingId: string, idempotencyKey: string): Promise<PayResult> {
  return request<PayResult>(`/bookings/${bookingId}/pay`, {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
  });
}
