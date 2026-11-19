import type { Metadata, Viewport } from "next";
// Self-hosted fonts (no network at build time): Instrument Sans 400/500/600, Plex Mono for data
import "@fontsource/instrument-sans/400.css";
import "@fontsource/instrument-sans/500.css";
import "@fontsource/instrument-sans/600.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import Providers from "@/components/Providers";
import "./globals.css";

export const metadata: Metadata = {
  title: { default: "Strata Console", template: "%s · Strata" },
  description: "Operator console for Strata: decisions, reviews, mandates, agents and the ledger.",
  robots: { index: false, follow: false },
};

export const viewport: Viewport = {
  themeColor: "#2B59FF",
  width: "device-width",
  initialScale: 1,
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // The session sets data-theme on the client; suppress the attribute mismatch warning
    <html lang="en-GB" data-theme="light" suppressHydrationWarning>
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
