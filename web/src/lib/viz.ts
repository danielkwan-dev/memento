/**
 * Chart color tokens.
 *
 * These are NOT eyeballed. The categorical slots are the validated dark-mode
 * palette, checked against the #14181c chart surface with the dataviz
 * validator (lightness band, chroma floor, CVD separation, normal-vision floor,
 * contrast). Letterboxd's own green (#00e054) is deliberately NOT used for data
 * marks -- it sits at L 0.79, well outside the band for this surface -- so it is
 * reserved for UI chrome (buttons, the progress bar) where it carries no data
 * identity.
 *
 * Slot order is fixed and never cycled: reordering these hues puts orange next
 * to yellow, which fails CVD separation.
 */
export const SERIES = [
  '#3987e5', // 1 blue
  '#d95926', // 2 orange
  '#199e70', // 3 aqua
  '#c98500', // 4 yellow
  '#d55181', // 5 magenta
] as const

/** Single-series default. Most charts here show one measure. */
export const PRIMARY = SERIES[0]

/**
 * Sequential ramp (one hue, light->dark) for the calendar heatmap, where
 * magnitude is the encoding. The lightest step may recede toward the surface
 * because it means "nothing watched".
 */
export const SEQUENTIAL = [
  '#1b2127', // zero: reads as surface
  '#184f95',
  '#256abf',
  '#2a78d6',
  '#3987e5',
  '#5598e7',
  '#86b6ef',
] as const

/** Picks a sequential step for a value within [0, max]. */
export function heatStep(value: number, max: number): string {
  if (value <= 0 || max <= 0) return SEQUENTIAL[0]
  // 6 non-zero steps.
  const idx = Math.min(Math.ceil((value / max) * (SEQUENTIAL.length - 1)), SEQUENTIAL.length - 1)
  return SEQUENTIAL[idx]
}

export const AXIS = 'var(--color-muted)'
export const GRID = 'var(--color-border)'

/**
 * Letterboxd's own accent trio, used the way the reference project uses them:
 * ONE per chart, as that chart's identity colour.
 *
 * These deliberately are not a categorical scale. The accessibility problem with
 * them is telling several apart *within one chart* -- they sit too close in
 * lightness for that. A chart drawn in a single colour has nothing to confuse, so
 * the brand palette is safe here, and it is what makes the dashboard look like it
 * belongs to Letterboxd rather than a generic BI tool.
 */
export const LBX = {
  green: '#00e054',
  blue: '#40bbf4',
  orange: '#ff8000',
} as const

/** A muted counterpart for the "other"/remainder slice of a two-part chart. */
export const LBX_MUTED = '#2a333c'
