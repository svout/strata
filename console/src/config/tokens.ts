/**
 * Strata design tokens for TypeScript (MUI palette, SVG fills).
 * Mirrors src/app/globals.css; source of truth is the vault note
 * "Design/Design Tokens.md". Change both files together.
 */
export const primitives = {
  cobalt: {
    50: "#F0F3FF",
    100: "#DCE4FF",
    200: "#B9C8FF",
    300: "#8FA6FF",
    400: "#5E80FF",
    500: "#2B59FF",
    600: "#2249E8",
    700: "#1A38BF",
    900: "#0F1F66",
  },
  ink: "#0B0D12",
  stone: "#E9E7E1",
  paper: "#F7F6F2",
  white: "#FFFFFF",
  graphite: "#2A2D35",
  slate: "#55575E",
  mist: "#A3A6AE",
  line: "#DEDCD5",
  amber: "#F2C14E",
  mint: "#3CCB9F",
} as const;

/** Semantic tokens, light mode. */
export const semantic = {
  bgCanvas: "#E9E7E1",
  bgSurface: "#F7F6F2",
  bgElevated: "#FFFFFF",
  borderDefault: "#DEDCD5",
  textPrimary: "#0B0D12",
  textSecondary: "#55575E",
  brandPrimary: "#2B59FF",
  brandPrimaryHover: "#2249E8",
  brandSubtle: "#DCE4FF",
  brandOnSubtle: "#1A38BF",
  signalApprove: "#0E8A6A",
  signalReview: "#E0A100",
  signalBlock: "#C4262E",
} as const;

/** Shifted-pill pattern colours: base + pill (base +~8% lightness). */
export const patternColours = {
  cobalt: { base: "#2B59FF", pill: "#4169FF" },
  ink: { base: "#0B0D12", pill: "#161B2E" },
  stone: { base: "#F7F6F2", pill: "#E6E3DB" },
} as const;

/** Lit-pill colours and what they mean (PatternSystem board). */
export const litColours = {
  white: "#FFFFFF", // payment in flight (on cobalt)
  amber: "#F2C14E", // sent to review, max 1
  ink: "#0B0D12", // blocked, max 1
  cobalt: "#2B59FF", // active / checked (on ink and stone)
  mint: "#3CCB9F", // 200 OK, developer pages only
} as const;

export const radius = { sm: 8, md: 14, lg: 22, pill: 999 } as const;
