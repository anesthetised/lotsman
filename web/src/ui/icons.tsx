// Inline SVG icons: no icon font or external asset. Decorative, so hidden
// from assistive technology; the control carrying one supplies its name.

export function CloseIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M4 4l8 8M12 4l-8 8" />
    </svg>
  );
}

export function ChevronDownIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M4 6l4 4 4-4" />
    </svg>
  );
}

export function GlobeIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-5" fill="none" stroke="currentColor" stroke-width="1.25">
      <circle cx="8" cy="8" r="6" />
      <ellipse cx="8" cy="8" rx="2.5" ry="6" />
      <path d="M2 8h12" />
    </svg>
  );
}

export function SunIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-5" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round">
      <circle cx="8" cy="8" r="3" />
      <path d="M8 1.5v1m0 11v1m-6.5-6.5h1m11 0h1M3.4 3.4l.7.7m7.8 7.8.7.7m-9.2 0 .7-.7m7.8-7.8.7-.7" />
    </svg>
  );
}

export function MoonIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-5" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linejoin="round">
      <path d="M7.4 2a6 6 0 1 0 6.6 6.6A5 5 0 0 1 7.4 2Z" />
    </svg>
  );
}

export function CheckIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M3.5 8.5l3 3 6-7" />
    </svg>
  );
}

export function ArrowUpIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M8 13V3M4 7l4-4 4 4" />
    </svg>
  );
}

export function ArrowDownIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M8 3v10M4 9l4 4 4-4" />
    </svg>
  );
}

export function WarningIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" class="size-4 shrink-0" fill="none" stroke="currentColor" stroke-width="1.5">
      <path d="M8 2l6.5 11.5h-13zM8 6.5v3.5M8 11.5v.5" />
    </svg>
  );
}
