// A live time-series chart: a thin wrapper that owns one uPlot instance and
// pushes new data into it. uPlot draws on a canvas, so the chart is hidden
// from assistive technology; the caption carries the title and the latest values.

import "uplot/dist/uPlot.min.css";
import type { JSX } from "@solidjs/web";
import { createEffect, onSettled, untrack } from "solid-js";
import uPlot from "uplot";
import { useI18n } from "../i18n";
import { loneValues } from "./loneValues";

export interface ChartSeries {
  label: string;
  /** A colour token from styles.css, such as "--color-chart-down". */
  color: string;
  /**
   * Whether the series is sparse, with gaps (null) between values: a line
   * needs two values in a row, so a value alone between gaps gets a dot.
   */
  sparse?: boolean;
}

export interface LineChartProps {
  /** Columns in uPlot's layout: `[unix seconds, ...one array per series]`; null is a gap. */
  data: (number | null)[][];
  series: readonly ChartSeries[];
  /** Formats y values on the axis. */
  format: (value: number) => string;
  caption: JSX.Element;
  height?: number;
  /** "clock": a live window of seconds to minutes (the default); "date": history over hours to months. */
  axis?: "clock" | "date";
}

export function LineChart(props: LineChartProps) {
  const { locale } = useI18n();
  const clock = new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const hours = new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit" });
  const days = new Intl.DateTimeFormat(locale, { month: "short", day: "numeric" });
  // uPlot keeps ticks 40 px apart by default, which crowds 12-hour labels
  // such as "04:29:40 PM" into each other; leave room for the widest one.
  const sample = new Date(2000, 11, 28, 23, 58, 58);
  const space = (...formats: Intl.DateTimeFormat[]) => {
    const ctx = document.createElement("canvas").getContext("2d");
    if (!ctx) return 100;
    ctx.font = "12px system-ui, sans-serif";
    return Math.ceil(Math.max(...formats.map((f) => ctx.measureText(f.format(sample)).width))) + 24;
  };
  let el!: HTMLDivElement;
  let plot: uPlot | undefined;

  const options = (width: number): uPlot.Options => {
    const style = getComputedStyle(el);
    const token = (name: string) => style.getPropertyValue(name).trim();
    const axis: uPlot.Axis = {
      stroke: token("--color-muted"),
      grid: { stroke: token("--color-line"), width: 1 },
      ticks: { stroke: token("--color-line"), width: 1 },
    };
    // History: ticks a day or more apart show the date, closer ones the time
    // of day. Live: whole-second ticks in the user's clock format.
    const timeAxis: uPlot.Axis =
      props.axis === "date"
        ? { space: space(hours, days), values: (_u, ticks, _i, _space, incr) => ticks.map((s) => (incr >= 86_400 ? days : hours).format(s * 1000)) }
        : { space: space(clock), incrs: [1, 5, 10, 15, 30, 60, 120, 300, 600, 900], values: (_u, ticks) => ticks.map((s) => clock.format(s * 1000)) };
    return {
      width,
      height: props.height ?? 200,
      legend: { show: false },
      cursor: { show: false },
      scales: { y: { range: (_u, _min, max) => [0, Math.max(max, 1)] } },
      axes: [
        { ...axis, ...timeAxis },
        { ...axis, size: 96, values: (_u, ticks) => ticks.map(props.format) },
      ],
      series: [
        {},
        ...props.series.map(
          (s): uPlot.Series => ({
            label: s.label,
            stroke: token(s.color),
            width: 1.5,
            points: s.sparse
              ? { show: true, size: 5, width: 0, fill: token(s.color), filter: (u, i) => loneValues(u.data[i] ?? []) }
              : { show: false },
          }),
        ),
      ],
    };
  };

  onSettled(() => {
    // Colours come from CSS, so a theme switch rebuilds the plot.
    const dark = matchMedia("(prefers-color-scheme: dark)");
    const build = () => {
      plot?.destroy();
      plot = new uPlot(options(el.clientWidth), untrack(() => props.data) as uPlot.AlignedData, el);
    };
    const resize = new ResizeObserver(() => plot?.setSize({ width: el.clientWidth, height: props.height ?? 200 }));
    build();
    resize.observe(el);
    dark.addEventListener("change", build);
    return () => {
      resize.disconnect();
      dark.removeEventListener("change", build);
      plot?.destroy();
    };
  });

  createEffect(
    () => props.data,
    (data) => plot?.setData(data as uPlot.AlignedData),
  );

  return (
    <figure class="min-w-0">
      <figcaption class="mb-2 text-sm">{props.caption}</figcaption>
      <div ref={el} aria-hidden="true" />
    </figure>
  );
}
