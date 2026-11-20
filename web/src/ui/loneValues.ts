// Which values of a sparse chart series need a dot: uPlot imports touch the
// DOM, so this lives apart from LineChart.tsx and its tests need none.

/** The indices of values with a gap (null) or the edge on both sides: no line reaches them. */
export function loneValues(values: ArrayLike<number | null | undefined>): number[] {
  const gap = (i: number) => values[i] === null || values[i] === undefined;
  const lone = [];
  for (let i = 0; i < values.length; i++) if (!gap(i) && gap(i - 1) && gap(i + 1)) lone.push(i);
  return lone;
}
