"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";

import { useAuth } from "@/lib/auth-context";

export function SiteHeader() {
  const { user, loading, logout } = useAuth();
  const router = useRouter();

  return (
    <header className="border-b border-ink-800 bg-ink-900/80 backdrop-blur">
      <div className="mx-auto flex w-full max-w-7xl items-center justify-between px-6 py-4">
        <Link href="/events" className="flex items-center gap-2.5">
          <span className="grid h-7 w-7 place-items-center rounded bg-accent text-sm font-bold text-ink-950">
            S
          </span>
          <span className="text-lg font-semibold tracking-tight text-ink-200">SeatSync</span>
        </Link>

        <nav className="flex items-center gap-1">
          <Link href="/events" className="btn-ghost">
            Events
          </Link>

          {loading ? (
            // A fixed-width placeholder keeps the header from jumping while
            // the session is restored.
            <span className="h-9 w-32" aria-hidden />
          ) : user ? (
            <>
              <Link href="/me/bookings" className="btn-ghost">
                My bookings
              </Link>
              <span className="ml-2 hidden text-sm text-ink-400 sm:inline">{user.name}</span>
              <button
                type="button"
                className="btn-ghost"
                onClick={async () => {
                  await logout();
                  router.push("/events");
                }}
              >
                Sign out
              </button>
            </>
          ) : (
            <>
              <Link href="/login" className="btn-ghost">
                Sign in
              </Link>
              <Link href="/register" className="btn-primary">
                Create account
              </Link>
            </>
          )}
        </nav>
      </div>
    </header>
  );
}
