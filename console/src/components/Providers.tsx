"use client";

import type { ReactNode } from "react";
import { AppRouterCacheProvider } from "@mui/material-nextjs/v15-appRouter";
import { ThemeProvider } from "@mui/material/styles";
import { theme } from "@/lib/theme";
import { SessionProvider } from "@/lib/session";
import { ToastProvider } from "./ui/Toasts";
import { PrototypeNotice } from "./shell/PrototypeNotice";

/**
 * Same MUI setup as strata-marketing: Emotion styles go into `@layer mui`
 * so Tailwind classes always win. No CssBaseline: Tailwind's preflight is
 * the reset.
 *
 * Every route renders inside this, so the PrototypeNotice placed here is on
 * every screen. The column wrapper keeps the pages' h-full/min-h-full
 * layouts filling what is left below it, rather than overflowing.
 */
export default function Providers({ children }: { children: ReactNode }) {
  return (
    <AppRouterCacheProvider options={{ enableCssLayer: true }}>
      <ThemeProvider theme={theme}>
        <SessionProvider>
          <ToastProvider>
            <div className="flex h-full flex-col">
              <PrototypeNotice />
              <div className="min-h-0 flex-1">{children}</div>
            </div>
          </ToastProvider>
        </SessionProvider>
      </ThemeProvider>
    </AppRouterCacheProvider>
  );
}
