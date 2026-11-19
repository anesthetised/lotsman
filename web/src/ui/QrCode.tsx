// A QR code as an inline SVG path: no canvas, no image and no data: URL, so
// it needs nothing from the CSP beyond the page itself. Dark modules on
// white with the standard four-module quiet zone, in either theme, because
// scanners expect that.

import { correction, generate } from "lean-qr";
import { toSvgPath } from "lean-qr/extras/svg";
import { createMemo, Show } from "solid-js";

const QUIET = 4;

/** The code's size in modules and its SVG path; undefined when the text does not fit a QR code. */
export function qrCode(text: string): { size: number; path: string } | undefined {
  try {
    const code = generate(text, { minCorrectionLevel: correction.L });
    return { size: code.size, path: toSvgPath(code) };
  } catch {
    return undefined;
  }
}

export interface QrCodeProps {
  text: string;
  /** Accessible name of the image. */
  label: string;
  /** Shown instead when the text is too long for a QR code. */
  tooLong: string;
  /** Width limit and placement; the default is at most 16rem. */
  class?: string;
}

export function QrCode(props: QrCodeProps) {
  const code = createMemo(() => qrCode(props.text));
  return (
    <Show when={code()} fallback={<p class="text-sm text-muted">{props.tooLong}</p>}>
      {(c) => (
        <svg
          role="img"
          aria-label={props.label}
          viewBox={`${-QUIET} ${-QUIET} ${c().size + 2 * QUIET} ${c().size + 2 * QUIET}`}
          shape-rendering="crispEdges"
          class={["aspect-square w-full rounded-md", props.class ?? "max-w-64"]}
        >
          <rect x={-QUIET} y={-QUIET} width={c().size + 2 * QUIET} height={c().size + 2 * QUIET} fill="#fff" />
          <path d={c().path} fill="#000" />
        </svg>
      )}
    </Show>
  );
}
