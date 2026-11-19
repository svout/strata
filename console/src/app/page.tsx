"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useSession } from "@/lib/session";

/** P1 routing rule: not signed in → sign-in; no workspace set up → onboarding; else Overview. */
export default function Home() {
  const s = useSession();
  const router = useRouter();
  useEffect(() => {
    if (!s.ready) return;
    router.replace(!s.signedIn ? "/sign-in" : !s.onboarded ? "/onboarding" : "/overview");
  }, [s.ready, s.signedIn, s.onboarded, router]);
  return null;
}
