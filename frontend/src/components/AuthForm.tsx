"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";
import type { FormEvent } from "react";

import { ApiError } from "@/lib/api";
import { useAuth } from "@/lib/auth-context";

type Mode = "login" | "register";

export function AuthForm({ mode }: { mode: Mode }) {
  const { login, register } = useAuth();
  const router = useRouter();
  const searchParams = useSearchParams();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const isRegister = mode === "register";

  async function handleSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);

    try {
      if (isRegister) await register(email, password, name);
      else await login(email, password);

      // Return the user where they were headed before being asked to sign in.
      const next = searchParams.get("next");
      router.push(next && next.startsWith("/") ? next : "/events");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong. Please try again.");
      setSubmitting(false);
    }
  }

  return (
    <div className="mx-auto w-full max-w-sm py-12">
      <h1 className="text-2xl font-semibold tracking-tight text-ink-200">
        {isRegister ? "Create your account" : "Sign in"}
      </h1>
      <p className="mt-1.5 text-sm text-ink-400">
        {isRegister ? "You need an account to hold and buy seats." : "Welcome back."}
      </p>

      <form onSubmit={handleSubmit} className="mt-8 space-y-4" noValidate>
        {isRegister && (
          <div>
            <label htmlFor="name" className="label">
              Name
            </label>
            <input
              id="name"
              className="input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="name"
              required
            />
          </div>
        )}

        <div>
          <label htmlFor="email" className="label">
            Email
          </label>
          <input
            id="email"
            type="email"
            className="input"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            required
          />
        </div>

        <div>
          <label htmlFor="password" className="label">
            Password
          </label>
          <input
            id="password"
            type="password"
            className="input"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete={isRegister ? "new-password" : "current-password"}
            required
          />
          {isRegister && <p className="mt-1.5 text-xs text-ink-500">At least 8 characters.</p>}
        </div>

        {error && (
          <p role="alert" className="rounded-md border border-red-900/60 bg-red-950/40 px-3 py-2 text-sm text-red-300">
            {error}
          </p>
        )}

        <button type="submit" className="btn-primary w-full" disabled={submitting}>
          {submitting ? "Please wait…" : isRegister ? "Create account" : "Sign in"}
        </button>
      </form>

      <p className="mt-6 text-center text-sm text-ink-400">
        {isRegister ? (
          <>
            Already have an account?{" "}
            <Link href="/login" className="text-accent hover:underline">
              Sign in
            </Link>
          </>
        ) : (
          <>
            No account yet?{" "}
            <Link href="/register" className="text-accent hover:underline">
              Create one
            </Link>
          </>
        )}
      </p>

      {!isRegister && (
        <div className="mt-8 rounded-md border border-ink-800 bg-ink-900 px-4 py-3">
          <p className="text-xs font-medium uppercase tracking-wide text-ink-500">Demo account</p>
          <p className="mt-1.5 font-mono text-xs text-ink-300">demo@seatsync.dev / password123</p>
        </div>
      )}
    </div>
  );
}
