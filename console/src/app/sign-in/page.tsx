"use client";

import { Suspense, useEffect } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import { Logo } from "@/components/brand";
import { useSession } from "@/lib/session";
import { IS_SAMPLE_DATA } from "@/api";

/** P1 · Get in with one click. Google or GitHub only. No workspace yet → P2, else → P3. */
export default function SignInPage() {
  return (
    <Suspense fallback={null}>
      <SignIn />
    </Suspense>
  );
}

/** Only same-site paths are followed, so ?next= can't send anyone to another website. */
const safeNext = (v: string | null) => (v && v.startsWith("/") && !v.startsWith("//") ? v : "/overview");

function SignIn() {
  const s = useSession();
  const router = useRouter();
  const next = safeNext(useSearchParams().get("next"));
  useEffect(() => {
    if (s.ready && s.signedIn) router.replace(s.onboarded ? next : "/onboarding");
  }, [s.ready, s.signedIn, s.onboarded, router, next]);

  const signIn = () => {
    s.signIn();
    router.replace(s.onboarded ? next : "/onboarding");
  };

  return (
    <div className="grid min-h-full lg:grid-cols-2">
      <main className="flex items-center justify-center bg-elevated px-6 py-16">
        <div className="flex w-full max-w-[380px] flex-col gap-5">
          <Logo size={34} />
          <h1 className="mt-4 text-[36px] leading-[1.05] font-semibold tracking-[-0.035em]">Sign in to Strata</h1>
          <p className="text-[16px] text-fg-muted">Use your work account.</p>
          {/* Provider marks go here from the Google and GitHub brand kits once real OAuth is wired */}
          <Button variant="outline" size="large" className="!h-[52px] !text-[16px]" onClick={signIn}>
            Continue with Google
          </Button>
          <Button variant="ink" size="large" className="!h-[52px] !text-[16px]" onClick={signIn}>
            Continue with GitHub
          </Button>
          <p className="text-[13px] leading-normal text-fg-muted">
            By continuing you agree to the [Terms] and [Privacy policy].
            {IS_SAMPLE_DATA && " This build uses sample data: both buttons open the demo workspace without contacting Google or GitHub."}
          </p>
        </div>
      </main>
      <div data-theme="dark" className="relative hidden flex-col justify-end overflow-hidden bg-cobalt-500 p-16 text-white lg:flex">
        <svg className="absolute inset-0 h-full w-full" aria-hidden>
          <defs>
            <pattern id="signin-pills" width="96" height="96" patternUnits="userSpaceOnUse">
              <rect x="36" y="14" width="48" height="18" rx="9" fill="#3A63FF" />
              <rect x="24" y="39" width="48" height="18" rx="9" fill="#3A63FF" />
              <rect x="12" y="64" width="48" height="18" rx="9" fill="#3A63FF" />
            </pattern>
          </defs>
          <rect width="100%" height="100%" fill="url(#signin-pills)" />
        </svg>
        <div className="relative flex flex-col gap-3 rounded-lg bg-cobalt-500 p-6">
          <p className="text-[40px] leading-[1.05] font-semibold tracking-[-0.04em]">Every agent payment, checked in three layers.</p>
          <p className="font-mono text-[13px] text-cobalt-100">MANDATE · RISK · LEDGER</p>
        </div>
      </div>
    </div>
  );
}
