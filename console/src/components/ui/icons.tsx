/** Icons: 24 px grid, 1.75 stroke, round caps (Brand Guidelines → Graphic language). */
const PATHS = {
  overview: "M4 13h6V4H4zM14 20h6v-9h-6zM4 20h6v-3H4zM14 7h6V4h-6z",
  decisions: "M4 6h16M4 12h16M4 18h10",
  review: "M12 8v5l3 2M12 20a8 8 0 1 0 0-16 8 8 0 0 0 0 16z",
  mandates: "M7 3h7l5 5v13H7zM14 3v5h5M10 13h6M10 17h4",
  agents: "M7 8h10a3 3 0 0 1 3 3v5a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3v-5a3 3 0 0 1 3-3zM12 4v4M9.5 13.5h.01M14.5 13.5h.01",
  ledger: "M5 4h14v16H5zM9 8h6M9 12h6M9 16h3",
  settings: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z",
  shield: "M12 3l8 4v5c0 4.5-3.4 8-8 9-4.6-1-8-4.5-8-9V7zM9 12l2 2 4-4",
  search: "M11 17.5a6.5 6.5 0 1 0 0-13 6.5 6.5 0 0 0 0 13zM20 20l-4-4",
  chevronDown: "M6 9l6 6 6-6",
  chevronLeft: "M15 6l-6 6 6 6",
  close: "M6 6l12 12M18 6L6 18",
  check: "M5 12l5 5 9-10",
  plus: "M12 5v14M5 12h14",
  play: "M8 5l11 7-11 7z",
  pause: "M9 5v14M15 5v14",
  key: "M8 19a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM11 12l9-9M17 6l3 3",
  copy: "M8 8h12v12H8zM16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3",
  sun: "M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4",
  moon: "M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z",
  menu: "M4 7h16M4 12h16M4 17h16",
  out: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
  refresh: "M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7",
  arrowRight: "M5 12h14M13 6l6 6-6 6",
  download: "M12 4v12M7 11l5 5 5-5M5 20h14",
  user: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21c1-4 4-6 8-6s7 2 8 6",
} as const;

export type IconName = keyof typeof PATHS;

export function Icon({ name, size = 18, className }: { name: IconName; size?: number; className?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden focusable="false" className={className}>
      <path d={PATHS[name]} />
    </svg>
  );
}
