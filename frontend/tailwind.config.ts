import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // A single dark palette, defined once so seat colours and chrome
        // stay in step.
        ink: {
          950: "#08090c",
          900: "#0d0f14",
          850: "#12151c",
          800: "#181c25",
          700: "#232834",
          600: "#333a4a",
          500: "#4a5364",
          400: "#6b7688",
          300: "#98a1b2",
          200: "#c5cbd6",
        },
        seat: {
          // Seat map states. These are the only place these colours appear,
          // so the legend and the map cannot drift apart.
          available: "#3f4756",
          hover: "#5b6577",
          selected: "#f5c542",
          mine: "#f5c542",
          taken: "#c0392b",
          sold: "#1b1f28",
        },
        accent: {
          DEFAULT: "#4f9cf9",
          hover: "#6cadfb",
          muted: "#1e3a5f",
        },
      },
      fontFamily: {
        sans: ["ui-sans-serif", "system-ui", "-apple-system", "Segoe UI", "Roboto", "Helvetica Neue", "Arial", "sans-serif"],
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
    },
  },
  plugins: [],
};

export default config;
