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
