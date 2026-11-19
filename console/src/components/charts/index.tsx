"use client";

import { BarChart } from "@mui/x-charts/BarChart";
import { SparkLineChart } from "@mui/x-charts/SparkLineChart";
import { useSession } from "@/lib/session";

/**
 * Strata's chart kit on top of MUI X Charts (MIT, community tier).
 *
 * Rules from the design (P3 board, Brand Guidelines → signal colours):
 * - Signal colours mean outcomes only: approve, review, block.
 * - On outcome charts, approved is cobalt 300 so reviews and blocks stand
 *   out ("Designer: approved in cobalt 300", P3).
 * - Every chart has a text label for screen readers, and every bar can be
 *   reached with the keyboard (MUI X's built-in focus handling).
 *
 * Axis, grid and tooltip colours come from CSS (globals.css → "Charts"), so
 * they follow light and dark mode. Series colours are set here per theme,
 * because SVG fill attributes can't read CSS variables reliably.
 */

const PALETTE = {
  light: { approved: "#8FA6FF", approve: "#0E8A6A", review: "#E0A100", block: "#C4262E", brand: "#2B59FF", muted: "#A3A6AE" },
  dark: { approved: "#5E80FF", approve: "#3CCB9F", review: "#F2C14E", block: "#FF7A7E", brand: "#8FA6FF", muted: "#55575E" },
} as const;

export function useChartColors() {
  const { theme } = useSession();
  return PALETTE[theme];
}

const count = (v: number | null) => (v == null ? "" : `${v.toLocaleString("en-GB")}`);

export type OutcomeBucket = { label: string; approve: number; review: number; block: number };

/** Stacked approved / review / blocked bars. Clicking a bar opens those payments. */
export function OutcomeBars({ buckets, height = 240, onSelect, label }: { buckets: OutcomeBucket[]; height?: number; onSelect?: (index: number) => void; label: string }) {
  const c = useChartColors();
  const every = Math.max(1, Math.ceil(buckets.length / 8));
  return (
    <div role="figure" aria-label={label} className={onSelect ? "strata-chart cursor-pointer" : "strata-chart"}>
      <BarChart
        height={height}
        margin={{ top: 8, right: 8, bottom: 0, left: 0 }}
        borderRadius={3}
        grid={{ horizontal: true }}
        hideLegend
        xAxis={[
          {
            scaleType: "band",
            data: buckets.map((b) => b.label),
            categoryGapRatio: 0.28,
            disableTicks: true,
            tickLabelInterval: (_v: unknown, i: number) => i % every === 0,
          },
        ]}
        yAxis={[{ width: 32, disableLine: true, disableTicks: true, tickMinStep: 1 }]}
        series={[
          { data: buckets.map((b) => b.approve), label: "Approved", stack: "o", color: c.approved, valueFormatter: count },
          { data: buckets.map((b) => b.review), label: "Review", stack: "o", color: c.review, valueFormatter: count },
          { data: buckets.map((b) => b.block), label: "Blocked", stack: "o", color: c.block, valueFormatter: count },
        ]}
        onAxisClick={onSelect ? (_e, d) => d && d.dataIndex != null && onSelect(d.dataIndex) : undefined}
      />
    </div>
  );
}

/** Horizontal bars for "which control fired", one colour, sorted by the caller. */
export function RankBars({ items, onSelect, label, height }: { items: { label: string; value: number }[]; onSelect?: (index: number) => void; label: string; height?: number }) {
  const c = useChartColors();
  return (
    <div role="figure" aria-label={label} className={onSelect ? "strata-chart cursor-pointer" : "strata-chart"}>
      <BarChart
        layout="horizontal"
        height={height ?? Math.max(120, items.length * 34 + 16)}
        margin={{ top: 0, right: 16, bottom: 0, left: 0 }}
        borderRadius={3}
        hideLegend
        grid={{ vertical: true }}
        yAxis={[{ scaleType: "band", data: items.map((i) => i.label), width: 170, disableTicks: true, disableLine: true, categoryGapRatio: 0.4 }]}
        xAxis={[{ disableTicks: true, disableLine: true, tickMinStep: 1, height: 24 }]}
        series={[{ data: items.map((i) => i.value), label: "Payments", color: c.muted, valueFormatter: count }]}
        onAxisClick={onSelect ? (_e, d) => d && d.dataIndex != null && onSelect(d.dataIndex) : undefined}
      />
    </div>
  );
}

/** Money per day (Agents page). */
export function MoneyBars({ labels, values, label, height = 200 }: { labels: string[]; values: number[]; label: string; height?: number }) {
  const c = useChartColors();
  const fmt = (v: number | null) => (v == null ? "" : `£${(v / 100).toLocaleString("en-GB", { maximumFractionDigits: 0 })}`);
  const every = Math.max(1, Math.ceil(labels.length / 7));
  return (
    <div role="figure" aria-label={label} className="strata-chart">
      <BarChart
        height={height}
        margin={{ top: 8, right: 8, bottom: 0, left: 0 }}
        borderRadius={3}
        grid={{ horizontal: true }}
        hideLegend
        xAxis={[{ scaleType: "band", data: labels, disableTicks: true, categoryGapRatio: 0.3, tickLabelInterval: (_v: unknown, i: number) => i % every === 0 }]}
        yAxis={[{ width: 52, disableLine: true, disableTicks: true, valueFormatter: (v: number) => fmt(v) }]}
        series={[{ data: values, label: "Spent", color: c.brand, valueFormatter: fmt }]}
      />
    </div>
  );
}

/** Before → after for the mandate replay: one stacked row per version. */
export function CompareBars({ rows, label }: { rows: { label: string; approve: number; review: number; block: number }[]; label: string }) {
  const c = useChartColors();
  return (
    <div role="figure" aria-label={label} className="strata-chart">
      <BarChart
        layout="horizontal"
        height={rows.length * 44 + 8}
        margin={{ top: 4, right: 8, bottom: 0, left: 0 }}
        borderRadius={3}
        hideLegend
        yAxis={[{ scaleType: "band", data: rows.map((r) => r.label), width: 64, disableTicks: true, disableLine: true, categoryGapRatio: 0.35 }]}
        xAxis={[{ position: "none" }]}
        series={[
          { data: rows.map((r) => r.approve), label: "Approved", stack: "o", color: c.approve, valueFormatter: count },
          { data: rows.map((r) => r.review), label: "Sent to review", stack: "o", color: c.review, valueFormatter: count },
          { data: rows.map((r) => r.block), label: "Blocked", stack: "o", color: c.block, valueFormatter: count },
        ]}
      />
    </div>
  );
}

/** Tiny 7-day trend for a stat tile. */
export function Spark({ data, labels, tone, label }: { data: number[]; labels: string[]; tone: "approved" | "approve" | "review" | "block" | "muted"; label: string }) {
  const c = useChartColors();
  return (
    <div role="img" aria-label={`${label}, last 7 days: ${data.join(", ")}`} className="strata-chart">
      <SparkLineChart
        data={data}
        plotType="bar"
        height={34}
        color={c[tone]}
        showTooltip
        showHighlight
        xAxis={{ scaleType: "band", data: labels }}
        valueFormatter={count}
      />
    </div>
  );
}
