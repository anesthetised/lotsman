import { createSignal, onSettled, Show } from "solid-js";
import { useConsoleI18n } from "../../i18n/console";

const units = { B: "byte", KB: "kilobyte", MB: "megabyte", GB: "gigabyte", TB: "terabyte" } as const;

export interface OutlineQuotaValue {
  limit: number;
  remaining: number;
  unit: keyof typeof units;
  updatedAt: string;
  stale: boolean;
}

/** A null entry is configured but has no sample yet. */
export function useOutlineUsage() {
  const [usage, setUsage] = createSignal<Record<string, OutlineQuotaValue | null | undefined>>({});
  onSettled(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function poll() {
      try {
        // POST supplies the Origin header that the panel requires on all API calls.
        const response = await fetch("/api/outline-usage", { method: "POST", signal: abort.signal });
        if (response.status === 204 || response.status === 401) return;
        if (response.ok && response.headers.get("Content-Type")?.includes("application/json")) {
          setUsage(await response.json());
        }
      } catch {
        // Quota polling is independent of live gateway status.
      }
      if (!abort.signal.aborted) timer = setTimeout(poll, 60_000);
    }
    void poll();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  });
  return usage;
}

export function OutlineQuota(props: { quota?: OutlineQuotaValue | null; labeled?: boolean }) {
  const i18n = useConsoleI18n();
  const { c, fmt } = i18n;
  const label = (quota: OutlineQuotaValue) => {
    const options = { maximumFractionDigits: 2 } as const;
    const remaining = new Intl.NumberFormat(i18n.locale, options).format(quota.remaining);
    const limit = new Intl.NumberFormat(i18n.locale, {
      ...options,
      style: "unit",
      unit: units[quota.unit],
      unitDisplay: "short",
    }).format(quota.limit);
    return c.outlineUsage.remaining(remaining, limit);
  };

  return (
    <Show when={props.quota !== undefined}>
      <div class="text-sm tabular-nums">
        <Show when={props.labeled}>
          <span class="mr-2 text-muted">{c.outlineUsage.title}</span>
        </Show>
        <Show when={props.quota} fallback={<span class="text-muted">{c.outlineUsage.unavailable}</span>}>
          {(quota) => (
            <>
              <span class="font-medium whitespace-nowrap" title={c.outlineUsage.updated(fmt.dateTime(new Date(quota().updatedAt)))}>
                {label(quota())}
              </span>
              <Show when={quota().stale}>
                <span class="ml-2 text-xs whitespace-nowrap text-warn">{c.outlineUsage.stale}</span>
              </Show>
            </>
          )}
        </Show>
      </div>
    </Show>
  );
}
