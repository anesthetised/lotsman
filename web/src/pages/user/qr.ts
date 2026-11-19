// A QR code as one SVG path, drawn from the config text. The encoder is
// pure JavaScript, and the output has no raster image, so the CSP needs no
// `data:` source.

import { encode } from "uqr";

/** The quiet zone around the symbol, in modules, as the QR standard asks. */
export const QUIET_ZONE = 4;

export interface QrShape {
  /** Modules per side, quiet zone included. */
  size: number;
  /** The dark modules, as horizontal runs. */
  path: string;
}

/**
 * Error correction is the lowest level: a screen shows a clean image, so the
 * spare capacity buys larger modules, which a phone camera reads more easily.
 * Throws when the text does not fit the largest symbol.
 */
export function qrShape(text: string): QrShape {
  const { data, size } = encode(text, { ecc: "L", border: 0 });
  const runs: string[] = [];
  data.forEach((row, y) => {
    for (let x = 0; x < size; x++) {
      if (!row[x]) continue;
      let end = x;
      while (row[end + 1]) end++;
      runs.push(`M${x + QUIET_ZONE} ${y + QUIET_ZONE}h${end - x + 1}v1h-${end - x + 1}z`);
      x = end;
    }
  });
  return { size: size + 2 * QUIET_ZONE, path: runs.join("") };
}
