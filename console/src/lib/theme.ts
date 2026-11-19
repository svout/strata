import { createTheme } from "@mui/material/styles";
import { primitives, semantic, radius } from "@/config/tokens";

/**
 * MUI theme built from the Strata tokens.
 *
 * Split of responsibilities:
 * - MUI owns interactive components (Button, Tabs, Menu, Drawer,
 *   Accordion, ToggleButton, Chip) and their states and accessibility.
 * - Tailwind owns layout, spacing and one-off styling.
 *
 * Colours in styleOverrides point at the CSS variables from globals.css,
 * so a MUI component placed in a dark section (data-theme="dark") picks
 * up the dark values automatically. The palette itself needs real hex
 * values because MUI computes hover and contrast colours from them.
 */
const v = (name: string) => `var(--color-${name})`;

const focusRing = {
  outline: `2px solid ${v("focus")}`,
  outlineOffset: 2,
};

const btnShadow = "0 1px 1px rgb(0 0 0 / 0.08), 0 0 0 1px rgb(26 31 54 / 0.04)";

export const theme = createTheme({
  palette: {
    primary: {
      main: semantic.brandPrimary,
      dark: primitives.cobalt[700],
      light: primitives.cobalt[400],
      contrastText: primitives.white,
    },
    secondary: { main: primitives.ink, contrastText: primitives.white },
    success: { main: semantic.signalApprove },
    warning: { main: semantic.signalReview },
    error: { main: semantic.signalBlock },
    text: { primary: semantic.textPrimary, secondary: semantic.textSecondary },
    background: { default: semantic.bgElevated, paper: semantic.bgElevated },
    divider: semantic.borderDefault,
  },
  shape: { borderRadius: radius.md },
  typography: {
    fontFamily: "var(--font-instrument-sans), ui-sans-serif, system-ui, sans-serif",
    button: { textTransform: "none", fontWeight: 600, letterSpacing: 0 },
  },
  components: {
    MuiButtonBase: {
      // Calm brand: no ripple, a clear focus ring instead
      defaultProps: { disableRipple: true },
      styleOverrides: { root: { "&.Mui-focusVisible": focusRing } },
    },
    MuiButton: {
      defaultProps: { variant: "primary", size: "medium", disableElevation: true },
      styleOverrides: {
        root: {
          borderRadius: 6,
          whiteSpace: "nowrap",
          boxShadow: btnShadow,
          fontWeight: 600,
          transition: "background-color .15s, border-color .15s, color .15s",
          "&.Mui-disabled": { opacity: 0.4 },
          variants: [
            {
              props: { variant: "primary" },
              style: {
                // Fill stays cobalt-500 even on ink: white on the dark-mode
                // brand token (#5E80FF) is below 4.5:1.
                backgroundColor: primitives.cobalt[500],
                color: primitives.white,
                boxShadow: `${btnShadow}, inset 0 -1px 0 rgb(0 0 0 / 0.12)`,
                "&:hover": { backgroundColor: primitives.cobalt[600] },
                "&:active": { backgroundColor: primitives.cobalt[700] },
              },
            },
            {
              props: { variant: "ink" },
              style: {
                backgroundColor: primitives.ink,
                color: primitives.white,
                "&:hover": { backgroundColor: primitives.graphite },
              },
            },
            {
              props: { variant: "white" },
              style: {
                backgroundColor: primitives.white,
                color: primitives.ink,
                "&:hover": { backgroundColor: primitives.cobalt[50] },
              },
            },
            {
              props: { variant: "outline" },
              style: {
                color: v("fg"),
                backgroundColor: v("elevated"),
                border: `1px solid ${v("line")}`,
                "&:hover": { borderColor: v("mist"), backgroundColor: v("surface") },
              },
            },
            {
              props: { variant: "ghost" },
              style: {
                backgroundColor: "transparent",
                color: v("fg"),
                boxShadow: "none",
                "&:hover": { backgroundColor: "rgb(127 127 127 / 0.1)" },
              },
            },
          ],
        },
        // Sizes from the UI kit: L 48 · M 40 · S 32
        sizeLarge: { height: 40, padding: "0 16px", fontSize: 15 },
        sizeMedium: { height: 32, padding: "0 12px", fontSize: 14 },
        sizeSmall: { height: 28, padding: "0 10px", fontSize: 13 },
      },
    },
    MuiIconButton: {
      styleOverrides: { root: { color: "inherit", width: 32, height: 32, borderRadius: 6 } },
    },
    MuiChip: {
      styleOverrides: {
        root: { borderRadius: 4, fontWeight: 600, fontFamily: "inherit" },
      },
    },
    MuiTabs: {
      styleOverrides: {
        root: { minHeight: 0 },
        indicator: { display: "none" },
      },
    },
    MuiTab: {
      styleOverrides: {
        root: {
          minHeight: 0,
          minWidth: 0,
          padding: "6px 12px",
          borderRadius: radius.sm,
          fontSize: 13,
          fontWeight: 500,
          color: v("fg-muted"),
          "&.Mui-selected": { color: v("fg"), backgroundColor: v("line") },
        },
      },
    },
    MuiToggleButton: {
      styleOverrides: {
        root: {
          height: 32,
          padding: "0 12px",
          border: `1px solid ${v("line")} !important`,
          borderRadius: "6px !important",
          fontSize: 13,
          fontWeight: 500,
          textTransform: "none",
          color: v("fg"),
          backgroundColor: v("elevated"),
          "&:hover": { backgroundColor: v("surface") },
          "&.Mui-selected, &.Mui-selected:hover": {
            backgroundColor: v("brand-faint"),
            borderColor: `${v("brand")} !important`,
            color: v("brand-on-subtle"),
          },
        },
      },
    },
    MuiToggleButtonGroup: {
      styleOverrides: { root: { gap: 8, flexWrap: "wrap" } },
    },
    MuiMenu: {
      styleOverrides: {
        paper: { borderRadius: 8, boxShadow: "var(--shadow-pop)" },
      },
    },
    MuiMenuItem: {
      styleOverrides: { root: { fontSize: 14, minHeight: 34 } },
    },
    MuiAccordion: {
      defaultProps: { disableGutters: true, elevation: 0, square: true },
      styleOverrides: {
        root: {
          background: "transparent",
          color: "inherit",
          borderTop: `1px solid ${v("line")}`,
          "&::before": { display: "none" },
        },
      },
    },
    MuiAccordionSummary: {
      styleOverrides: { root: { padding: 0, minHeight: 52 } },
    },
    MuiAccordionDetails: {
      styleOverrides: { root: { padding: "0 0 16px" } },
    },
    MuiDrawer: {
      styleOverrides: { paper: { width: "min(100vw, 360px)" } },
    },
  },
});
