"use client";

// Shares the signed-in user across the app and restores a session on load.

import { createContext, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";

import * as api from "./api";
import type { User } from "./types";

interface AuthContextValue {
  user: User | null;
  /** True until the initial session restore has settled. */
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  register: (email: string, password: string, name: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(api.getCurrentUser());
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    // Tokens are held in memory, so a reload starts with no access token and
    // has to exchange the stored refresh token before anything else runs.
    let cancelled = false;

    api
      .restoreSession()
      .then((restored) => {
        if (!cancelled) setUser(restored);
      })
      .catch(() => {
        if (!cancelled) setUser(null);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => api.onAuthChange(setUser), []);

  const value = useMemo<AuthContextValue>(
    () => ({
      user,
      loading,
      login: async (email, password) => {
        setUser(await api.login(email, password));
      },
      register: async (email, password, name) => {
        setUser(await api.register(email, password, name));
      },
      logout: async () => {
        await api.logout();
        setUser(null);
      },
    }),
    [user, loading],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) throw new Error("useAuth must be used inside an AuthProvider");
  return context;
}
