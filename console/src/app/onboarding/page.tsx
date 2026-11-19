"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import Button from "@mui/material/Button";
import clsx from "clsx";
import type { Category, Decision } from "@/api";
import { CATEGORIES } from "@/api/mock/catalogue";
import { Logo } from "@/components/brand";
import { LayerTrace, layersOf } from "@/components/decision/LayerTrace";
import { Field } from "@/components/ui";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { api } from "@/lib/useResource";
import { useSession } from "@/lib/session";
import { gbp, money, moneyShort } from "@/lib/format";

const STEPS = ["Workspace", "Connect agent", "First mandate", "Test payment"];

/** P2 · First blocked or approved test payment within one sitting. Four steps; a live plain-words summary on the right. */
export default function OnboardingPage() {
  const s = useSession();
  const router = useRouter();
  const toast = useToast();
  const [step, setStep] = useState(0);
  const [ws, setWs] = useState("");
  const [agentId, setAgentId] = useState("agt_");
  const [secret, setSecret] = useState<string | null>(null);
  const [cats, setCats] = useState<Category[]>(["flights", "hotels", "rail"]);
  const [perPayment, setPerPayment] = useState("800");
  const [perDay, setPerDay] = useState("2000");
  const [reviewOn, setReviewOn] = useState(true);
  const [reviewAbove, setReviewAbove] = useState("500");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<{ d: Decision; upTo: number } | null>(null);

  useEffect(() => {
    if (s.ready && !s.signedIn) router.replace("/sign-in");
  }, [s.ready, s.signedIn, router]);
  useEffect(() => {
    api().workspace().then((w) => setWs((v) => v || w.name));
  }, []);

  const pp = Number(perPayment), pd = Number(perDay), ra = Number(reviewAbove);
  const mandateOk = cats.length > 0 && pp > 0 && pd >= pp && (!reviewOn || (ra >= 0 && ra < pp));
  const sentence = `${agentId.length > 4 ? agentId : "Your agent"} may buy ${cats.length ? cats.join(", ") : "…"}, up to ${moneyShort(gbp(pp || 0))} each and ${moneyShort(gbp(pd || 0))} a day. ${reviewOn ? `Anything over ${moneyShort(gbp(ra || 0))} waits for you.` : "Nothing waits for a person."}`;

  const next = async () => {
    setError(null);
    setBusy(true);
    try {
      if (step === 0) await api().renameWorkspace(ws, s.user.name);
      if (step === 1 && !secret) {
        const r = await api().registerAgent({ id: agentId.trim(), description: "Connected during onboarding", owner: s.user.name }, s.user.name);
        setSecret(r.created.secret);
        setBusy(false);
        return; // stay on this step so the key can be copied
      }
      if (step === 2) {
        await api().saveMandate(
          null,
          {
            name: `${cats.map((c) => c[0].toUpperCase() + c.slice(1)).join(", ")}`,
            agentId: agentId.trim(),
            categories: cats,
            merchantIds: null,
            days: [0, 1, 2, 3, 4, 5, 6],
            hours: null,
            limits: { perPayment: gbp(pp), perDay: gbp(pd), perMonth: gbp(pd * 20) },
            review: { enabled: reviewOn, above: gbp(reviewOn ? ra : pp) },
          },
          s.user.name,
        );
      }
      setStep((x) => x + 1);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Something went wrong.");
    }
    setBusy(false);
  };

  const runTest = async () => {
    const d = await api().testPayment(agentId.trim());
    for (let i = 0; i < 3; i++) {
      setTest({ d, upTo: i });
      await new Promise((r) => setTimeout(r, 550));
    }
    setTest({ d, upTo: 3 });
  };

  const finish = () => {
    s.finishOnboarding();
    toast(`${agentId} is connected, has a mandate, and made its first payment.`);
    router.replace("/overview");
  };
  const skip = () => {
    s.finishOnboarding();
    toast("Setup skipped. You can register agents and add mandates any time.", "info");
    router.replace("/overview");
  };

  return (
    <div className="flex min-h-full flex-col bg-surface">
      <header className="flex h-[72px] shrink-0 items-center gap-4 border-b border-line bg-elevated px-6 md:px-10">
        <Logo size={28} />
        <ol className="mx-auto hidden items-center gap-2 text-[13px] font-semibold md:flex" aria-label="Progress">
          {STEPS.map((label, i) => (
            <li
              key={label}
              aria-current={i === step ? "step" : undefined}
              className={clsx("rounded-md px-2.5 py-1", i < step ? "bg-approve-tint text-approve-fg" : i === step ? "bg-brand-faint text-brand-on-subtle" : "bg-neutral-tint text-fg-muted")}
            >
              {i + 1} {label}
              {i < step && " ✓"}
            </li>
          ))}
        </ol>
        <button type="button" className="ml-auto text-[14px] text-fg-muted hover:underline md:ml-0" onClick={skip}>
          Skip for now
        </button>
      </header>

      <div className="flex flex-1 flex-col items-center justify-center gap-8 px-5 py-12 lg:flex-row lg:items-start">
        <section className="flex min-h-[460px] w-full max-w-[600px] flex-col gap-5 rounded-lg border border-line bg-elevated p-9 shadow-[var(--shadow-card)]">
          <p className="font-mono text-[11px] text-fg-muted md:hidden">STEP {step + 1} OF 4</p>
          {step === 0 && (
            <>
              <Heading title="Name your workspace" sub="Usually your company or team. You can change it later." />
              <Field label="Workspace name">{(id) => <input id={id} className="field !h-11" value={ws} onChange={(e) => setWs(e.target.value)} />}</Field>
            </>
          )}
          {step === 1 && (
            <>
              <Heading title="Connect an agent" sub="Register the agent that will ask to spend. It gets an API key to sign its requests with." />
              <Field label="Agent ID" hint="agt_ then lowercase letters, digits or dashes." error={error}>
                {(id) => <input id={id} className="field !h-11 font-mono" value={agentId} disabled={!!secret} onChange={(e) => setAgentId(e.target.value)} />}
              </Field>
              {secret && (
                <div className="flex flex-col gap-2">
                  <p className="text-[13px] font-semibold">Its API key. Copy it now: you won&apos;t see it again.</p>
                  <code className="rounded-md border border-dashed border-mist bg-surface p-3.5 font-mono text-[13px] break-all select-all">{secret}</code>
                  <Button variant="outline" size="small" className="self-start" onClick={() => navigator.clipboard.writeText(secret).then(() => toast("Key copied.", "info"), () => toast("Your browser blocked copying.", "review"))}>
                    <Icon name="copy" size={15} className="mr-1.5" /> Copy key
                  </Button>
                </div>
              )}
            </>
          )}
          {step === 2 && (
            <>
              <Heading title="Set your first mandate" sub="Tell Strata what this agent is allowed to spend on. You can change it any time." />
              <div className="flex flex-col gap-2">
                <span className="text-[13px] font-semibold">What can it buy?</span>
                <div className="flex flex-wrap gap-2 text-[13px]">
                  {CATEGORIES.filter((c) => c !== "refunds").map((c) => {
                    const on = cats.includes(c);
                    return (
                      <button key={c} type="button" aria-pressed={on} onClick={() => setCats(on ? cats.filter((x) => x !== c) : [...cats, c])} className={clsx("rounded-md px-2.5 py-1", on ? "border border-brand bg-brand-faint text-brand-on-subtle" : "border border-line")}>
                        {c[0].toUpperCase() + c.slice(1)}
                        {on && " ✓"}
                      </button>
                    );
                  })}
                </div>
              </div>
              <div className="grid grid-cols-2 gap-3.5">
                <Field label="Max per payment (£)">{(id) => <input id={id} inputMode="decimal" className="field !h-11 font-mono" value={perPayment} onChange={(e) => setPerPayment(e.target.value)} />}</Field>
                <Field label="Max per day (£)" error={pd < pp ? "Can't be lower than the per-payment cap." : null}>
                  {(id) => <input id={id} inputMode="decimal" className="field !h-11 font-mono" value={perDay} onChange={(e) => setPerDay(e.target.value)} />}
                </Field>
              </div>
              <label className="flex flex-wrap items-center gap-2.5 text-[14px]">
                <input type="checkbox" className="size-[18px] accent-[var(--color-brand)]" checked={reviewOn} onChange={(e) => setReviewOn(e.target.checked)} />
                Send payments over
                <input aria-label="Review threshold" inputMode="decimal" className="field !h-9 !w-[90px] font-mono" value={reviewAbove} disabled={!reviewOn} onChange={(e) => setReviewAbove(e.target.value)} />
                to me for review
              </label>
              {error && <p className="text-[12.5px] font-medium text-block-fg">{error}</p>}
            </>
          )}
          {step === 3 && (
            <>
              <Heading title="Try a test payment" sub={`Send one small payment from ${agentId} and watch it go through the three checks.`} />
              {!test ? (
                <Button variant="primary" size="large" className="self-start" onClick={runTest}>
                  <Icon name="play" size={16} className="mr-1.5" /> Send a test payment
                </Button>
              ) : (
                <div className="flex flex-col gap-3">
                  <p className="font-mono text-[14px]">{money(test.d.amount)}</p>
                  <LayerTrace layers={layersOf(test.d)} upTo={test.upTo < 3 ? test.upTo : undefined} />
                  {test.upTo >= 3 && <p className="rounded-md bg-approve-tint p-3.5 text-[14px] text-approve-fg">{test.d.explanation}</p>}
                </div>
              )}
            </>
          )}
          <div className="mt-auto flex justify-between pt-4">
            <Button variant="outline" size="large" disabled={step === 0 || busy} onClick={() => setStep((x) => x - 1)}>
              Back
            </Button>
            {step < 3 ? (
              <Button variant="primary" size="large" disabled={busy || (step === 0 && !ws.trim()) || (step === 1 && agentId.length < 7) || (step === 2 && !mandateOk)} onClick={next}>
                {step === 1 && !secret ? "Register agent" : "Save and continue"}
              </Button>
            ) : (
              <Button variant="primary" size="large" disabled={!test || test.upTo < 3} onClick={finish}>
                Go to overview
              </Button>
            )}
          </div>
        </section>

        <aside className="flex w-full max-w-[340px] flex-col gap-4 self-center lg:self-start">
          <div className="flex flex-col gap-2 rounded-lg border border-line bg-elevated p-5 shadow-[inset_3px_0_0_var(--color-brand)]">
            <span className="text-[12px] font-semibold text-fg-muted">In plain words · live</span>
            <p className="text-[16px] leading-[1.5] font-medium" aria-live="polite">
              {step === 0 ? `${ws || "Your workspace"} will control what your AI agents are allowed to spend.` : sentence}
            </p>
          </div>
          <p className="rounded-lg border border-line bg-elevated p-5 text-[13px] leading-normal text-fg-muted">This summary updates as you type. It&apos;s the same sentence the Mandates page shows, so people always read rules the same way.</p>
        </aside>
      </div>
    </div>
  );
}

function Heading({ title, sub }: { title: string; sub: string }) {
  return (
    <div className="flex flex-col gap-1.5">
      <h1 className="text-[30px] font-semibold tracking-[-0.01em]">{title}</h1>
      <p className="text-[15px] text-fg-muted">{sub}</p>
    </div>
  );
}
