import Link from "next/link";
import { Logo } from "@/components/brand";

export default function NotFound() {
  return (
    <main className="flex min-h-full flex-col items-start justify-center gap-4 px-8 py-16 md:px-24">
      <Logo size={28} />
      <h1 className="text-h1">This page doesn&apos;t exist</h1>
      <p className="text-fg-muted">The link may be old, or the record was never there.</p>
      <Link href="/overview" className="font-semibold text-brand-on-subtle">
        Go to Overview →
      </Link>
    </main>
  );
}
