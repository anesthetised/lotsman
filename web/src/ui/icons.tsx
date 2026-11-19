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
