import Link from "next/link";

export default function NotFound() {
  return (
    <div className="py-24 text-center">
      <h1 className="text-2xl font-semibold tracking-tight text-ink-200">Page not found</h1>
      <p className="mt-2 text-sm text-ink-400">That page does not exist.</p>
      <Link href="/events" className="btn-primary mt-6">
        Browse events
      </Link>
    </div>
  );
}
