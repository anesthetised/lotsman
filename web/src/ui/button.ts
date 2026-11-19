// Button styles, shared by our own buttons and the Kobalte triggers.

const base =
  "inline-flex items-center justify-center gap-1.5 rounded-md text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50";

const variants = {
  primary: "bg-accent px-3 py-1.5 text-accent-fg hover:opacity-90",
  secondary: "border border-line bg-surface px-3 py-1.5 hover:bg-bg",
  ghost: "p-1.5 text-muted hover:bg-bg hover:text-fg",
  /** For actions inside table rows. */
  compact: "border border-line bg-surface px-2 py-0.5 text-xs hover:bg-bg",
  danger: "bg-bad px-3 py-1.5 text-accent-fg hover:opacity-90",
};

export type ButtonVariant = keyof typeof variants;

export function buttonClass(variant: ButtonVariant = "secondary"): string[] {
  return [base, variants[variant]];
}
