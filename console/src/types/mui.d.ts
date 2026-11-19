import "@mui/material/Button";

/**
 * Strata button variants (Design Tokens → Core components → Button),
 * registered with MUI so <Button variant="primary"> is type-checked and
 * styled from the theme (src/lib/theme.ts).
 */
declare module "@mui/material/Button" {
  interface ButtonPropsVariantOverrides {
    primary: true; // cobalt fill
    ink: true; // ink fill ("Secondary" in the UI kit)
    white: true; // white fill, for the cobalt hero
    outline: true; // 1.5 px line, follows the section's text colour
    ghost: true; // text only
  }
}
