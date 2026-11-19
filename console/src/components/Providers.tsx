"use client";

import type { ReactNode } from "react";
import { AppRouterCacheProvider } from "@mui/material-nextjs/v15-appRouter";
import { ThemeProvider } from "@mui/material/styles";
import { theme } from "@/lib/theme";
import { SessionProvider } from "@/lib/session";
import { ToastProvider } from "./ui/Toasts";

/**
 * Same MUI setup as strata-marketing: Emotion styles go into `@layer mui`
 * so Tailwind classes always win. No CssBaseline: Tailwind's preflight is
 * the reset.
 */
export default function Providers({ children }: { children: ReactNode }) {
  return (
    <AppRouterCacheProvider options={{ enableCssLayer: true }}>
      <ThemeProvider theme={theme}>
        <SessionProvider>
          <ToastProvider>{children}</ToastProvider>
        </SessionProvider>
      </ThemeProvider>
    </AppRouterCacheProvider>
  );
}
