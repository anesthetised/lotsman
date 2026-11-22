// A device's route: countries in order or the fastest of them, their
// providers, and whether the device may fall back to another exit. Shared by
// the user page and the console; the page stores the route (`apply`) and
// words what failed. RouteFields are the controls alone, for a form that
// stores the route itself, such as adding a device. The editing rules are
// pure functions in route.ts.

import { equals } from "@bufbuild/protobuf";
import { createMemo, createSignal, createUniqueId, flush, For, Show } from "solid-js";
import type { UpstreamStatus } from "../gen/lotsman/v1/status_pb";
import { type Route, RouteSchema } from "../gen/lotsman/v1/user_pb";
import { useI18n } from "../i18n";
import type { Dict } from "../i18n/en";
import { buttonClass } from "../ui/button";
import { Chip, Segmented, Switch, SwitchField } from "../ui/Toggles";
import {
  blocked,
  changed,
  chosen,
  type CountryChoice,
  countryNow,
  DEFAULT_ROUTE,
  type Draft,
  draftOf,
  moveCountry,
  type Offer,
  offerOf,
  providerAvailable,
  routeOf,
  sameOffer,
  toggleCountry,
  toggleProvider,
} from "./route";

export interface RouteEditorProps {
  /** The stored route; unset is the default one. */
  route: Route | undefined;
  /** The exits the route may use, live: the user's allowed ones. */
  exits: readonly UpstreamStatus[];
  /** Whether providers show, as chips and in words. */
  providers: boolean;
  /** Shows the route without controls: the operator sets it. */
  readOnly?: boolean;
  /** Another change is under way; Apply waits for it. */
  busy?: boolean;
  /** Stores the route; resolves to a localized failure message accessor, or undefined once stored. */
  apply: (route: Route) => Promise<(() => string) | undefined>;
}

const action = [...buttonClass(), "min-h-11 px-4"];

export function RouteEditor(props: RouteEditorProps) {
  const { t, fmt } = useI18n();
  // The exits change every second; the draft only resets when the countries or providers on offer do.
  // Keep the draft’s country order when only its display language changes.
  const countryName = fmt.country;
  const offer = createMemo(() => offerOf(props.exits, countryName, props.providers), { equals: sameOffer });
  const saved = createMemo(() => props.route ?? DEFAULT_ROUTE, { equals: (a, b) => equals(RouteSchema, a, b) });
  // The edits, until the stored route changes.
  const [draft, setDraft] = createSignal(() => draftOf(saved(), offer()));
  const dirty = createMemo(() => changed(draft(), saved(), offer()));
  const [applying, setApplying] = createSignal(false);
  const [failure, setFailure] = createSignal<() => string>();

  const cancel = () => {
    setDraft(draftOf(saved(), offer()));
    setFailure(undefined);
  };
  const apply = async () => {
    setFailure(undefined);
    setApplying(true);
    const failed = await props.apply(routeOf(draft(), offer()));
    setApplying(false);
    setFailure(() => failed);
  };

  return (
    <Show when={offer().size > 0} fallback={<p class="text-sm text-muted">{t.route.noExits}</p>}>
      <div class="space-y-3 text-sm">
        <Show
          when={!props.readOnly}
          fallback={
            <>
              <RouteView draft={draft()} offer={offer()} exits={props.exits} />
              <NoExit draft={draft()} exits={props.exits} />
            </>
          }
        >
          <RouteFields draft={draft()} setDraft={setDraft} offer={offer()} exits={props.exits} />
          <Show when={failure()}>
            <p role="alert" class="rounded-md border border-bad p-3">
              {failure()?.()}
            </p>
          </Show>
          <div class="flex flex-wrap justify-end gap-2">
            <button type="button" class={action} disabled={!dirty() || applying()} onClick={cancel}>
              {t.route.cancel}
            </button>
            <button type="button" class={[...buttonClass("primary"), "min-h-11 px-4"]} disabled={!dirty() || applying() || props.busy} onClick={apply}>
              {t.route.apply}
            </button>
          </div>
          <p class="text-xs text-muted">{t.route.noReconnect}</p>
        </Show>
      </div>
    </Show>
  );
}

/**
 * The route of a device being added, for RouteFields in its form: it starts
 * as the default route, and route() stays unset until it is changed, so the
 * daemon's default (any exit, new ones included) applies unless the form
 * asked for another.
 */
export function createNewRoute(exits: () => readonly UpstreamStatus[], providers: () => boolean) {
  const { fmt } = useI18n();
  const countryName = fmt.country;
  const offer = createMemo(() => offerOf(exits(), countryName, providers()), { equals: sameOffer });
  const [draft, setDraft] = createSignal(() => draftOf(DEFAULT_ROUTE, offer()));
  return {
    offer,
    draft,
    setDraft,
    route: (): Route | undefined => (changed(draft(), DEFAULT_ROUTE, offer()) ? routeOf(draft(), offer()) : undefined),
    reset: () => setDraft(draftOf(DEFAULT_ROUTE, offer())),
  };
}

export interface RouteFieldsProps {
  draft: Draft;
  setDraft: (update: (draft: Draft) => Draft) => void;
  /** The countries and providers on offer (offerOf). */
  offer: Offer;
  /** The exits the route may use, live. */
  exits: readonly UpstreamStatus[];
}

/** The controls of a route: mode, countries with their providers and order, strict, and whether it leaves the device blocked. */
export function RouteFields(props: RouteFieldsProps) {
  const { t, fmt } = useI18n();
  const id = createUniqueId();
  const [announcement, setAnnouncement] = createSignal<() => string>();
  const [dragging, setDragging] = createSignal<string>();
  let list: HTMLOListElement | undefined;

  const handleId = (geo: string) => `${id}-move-${geo}`;

  /** Arrow keys move the focused country one place. */
  const onKey = (geo: string, e: KeyboardEvent) => {
    const by = e.key === "ArrowUp" ? -1 : e.key === "ArrowDown" ? 1 : 0;
    if (by === 0) return;
    e.preventDefault();
    const on = chosen(props.draft);
    const to = on.findIndex((c) => c.geo === geo) + by;
    if (to < 0 || to >= on.length) return;
    props.setDraft((d) => moveCountry(d, geo, to));
    setAnnouncement(() => () => t.route.moved(fmt.country(geo), to + 1, on.length));
    // Moving a keyed row moves its node, which drops focus: restore it once the move is in the DOM.
    flush();
    document.getElementById(handleId(geo))?.focus();
  };

  /** Dragging the handle (mouse, pen or touch) puts the country where the pointer is among the other chosen ones. */
  const onPointerDown = (geo: string, e: PointerEvent) => {
    if (e.button !== 0 || !list) return;
    e.preventDefault();
    const rows = list;
    const follow = (ev: PointerEvent) => {
      const others = [...rows.querySelectorAll<HTMLElement>("li[data-chosen]")].filter((li) => li.dataset.geo !== geo);
      const to = others.filter((li) => {
        const box = li.getBoundingClientRect();
        return box.top + box.height / 2 < ev.clientY;
      }).length;
      props.setDraft((d) => moveCountry(d, geo, to));
    };
    const stop = () => {
      window.removeEventListener("pointermove", follow);
      window.removeEventListener("pointerup", stop);
      window.removeEventListener("pointercancel", stop);
      setDragging(undefined);
    };
    setDragging(geo);
    window.addEventListener("pointermove", follow);
    window.addEventListener("pointerup", stop);
    window.addEventListener("pointercancel", stop);
  };

  const Row = (row: { choice: CountryChoice }) => {
    const c = () => row.choice;
    const name = () => fmt.country(c().geo);
    const position = () => chosen(props.draft).findIndex((x) => x.geo === c().geo);
    const providers = () => props.offer.get(c().geo) ?? [];
    return (
      <li
        data-geo={c().geo}
        data-chosen={c().on ? "" : undefined}
        class={[
          "grid items-center gap-x-2 py-1",
          props.draft.fastest ? "grid-cols-[minmax(0,1fr)_auto] px-3" : "grid-cols-[2.75rem_minmax(0,1fr)_auto] px-1.5",
          { "bg-bg": dragging() === c().geo },
        ]}
      >
        {/* Fastest has no order, so no position column. */}
        <Show when={!props.draft.fastest}>
          <Show when={c().on} fallback={<span aria-hidden="true" class="mx-auto size-6 rounded-md border border-dashed border-line" />}>
          <button
            type="button"
            id={handleId(c().geo)}
            class="grid min-h-11 min-w-11 cursor-grab touch-none place-items-center rounded-md active:cursor-grabbing"
            aria-label={t.route.position(name(), position() + 1, chosen(props.draft).length)}
            aria-describedby={`${id}-order`}
            onKeyDown={(e) => onKey(c().geo, e)}
            onPointerDown={(e) => onPointerDown(c().geo, e)}
          >
            <span class="grid size-6 place-items-center rounded-md border border-line bg-bg text-xs tabular-nums text-muted">{position() + 1}</span>
            </button>
          </Show>
        </Show>
        <div class={["min-w-0 py-1.5", { "opacity-50": !c().on }]}>
          <p class="flex flex-wrap items-baseline gap-x-2 font-medium">
            <span class="break-words">{name()}</span>
            <span class="text-xs font-normal tabular-nums text-muted">{latency(c(), props.exits, t, fmt.milliseconds)}</span>
          </p>
          <Show when={providers().length > 0}>
            <div role="group" aria-label={t.route.providers(name())} class="flex flex-wrap gap-x-1.5">
              <For each={providers()}>
                {(p) => {
                  const down = () => c().on && c().providers.includes(p) && !providerAvailable(c().geo, p, props.exits);
                  return (
                    <Chip
                      pressed={c().providers.includes(p)}
                      warn={down()}
                      disabled={!c().on || (c().providers.length === 1 && c().providers.includes(p))}
                      onChange={() => props.setDraft((d) => toggleProvider(d, c().geo, p, props.offer))}
                    >
                      {p}
                      <Show when={down()}>
                        <span class="sr-only">, {t.route.unavailable}</span>
                      </Show>
                    </Chip>
                  );
                }}
              </For>
            </div>
          </Show>
        </div>
        <Switch
          checked={c().on}
          label={t.route.use(name())}
          disabled={c().on && chosen(props.draft).length === 1}
          onChange={() => props.setDraft((d) => toggleCountry(d, c().geo, props.offer))}
        />
      </li>
    );
  };

  return (
    <>
      <div class="space-y-1.5">
        <Segmented
          label={t.route.mode}
          value={props.draft.fastest ? "fastest" : "ordered"}
          options={[
            { value: "ordered", label: t.route.ordered },
            { value: "fastest", label: t.route.fastest },
          ]}
          onChange={(mode) => props.setDraft((d) => ({ ...d, fastest: mode === "fastest" }))}
        />
        <p class="text-xs text-muted">{props.draft.fastest ? t.route.fastestHelp : t.route.orderedHelp}</p>
      </div>
      <ol ref={(el) => (list = el)} aria-label={t.route.countries} class="divide-y divide-line rounded-lg border border-line">
        <For each={props.draft.countries} keyed={(c) => c.geo}>
          {(c) => <Row choice={c()} />}
        </For>
      </ol>
      <Show when={!props.draft.fastest && chosen(props.draft).length > 1}>
        <p id={`${id}-order`} class="text-xs text-muted">
          {t.route.orderHelp}
        </p>
      </Show>
      <p aria-live="polite" class="sr-only">
        {announcement()?.()}
      </p>
      <SwitchField
        label={t.route.strict}
        hint={t.route.strictHelp}
        checked={props.draft.strict}
        onChange={(strict) => props.setDraft((d) => ({ ...d, strict }))}
      />
      <NoExit draft={props.draft} exits={props.exits} fix />
    </>
  );
}

/** The country's latency now, or that it is unavailable. */
function latency(c: CountryChoice, exits: readonly UpstreamStatus[], t: Dict, ms: (n: number) => string): string {
  const now = countryNow(c, exits);
  if (!now.available) return t.route.unavailable;
  return now.latencyMs === undefined ? "" : ms(now.latencyMs);
}

/** Warns that the route leaves the device blocked now; with fix, says how to change that. */
function NoExit(props: { draft: Draft; exits: readonly UpstreamStatus[]; fix?: boolean }) {
  const { t } = useI18n();
  return (
    <Show when={blocked(props.draft, props.exits)}>
      <p role="alert" class="rounded-md border border-warn p-3">
        <span class="font-semibold text-warn">{t.route.noExit}</span> {t.route.noExitWhy}
        <Show when={props.fix}> {t.route.noExitFix}</Show>
      </p>
    </Show>
  );
}

/** The route as text: what the user sees when only the operator may change it. */
function RouteView(props: { draft: Draft; offer: Offer; exits: readonly UpstreamStatus[] }) {
  const { t, fmt } = useI18n();
  return (
    <>
      <p>
        <span class="font-medium">{props.draft.fastest ? t.route.fastest : t.route.ordered}</span>
        <span class="text-muted"> · {props.draft.fastest ? t.route.fastestHelp : t.route.orderedHelp}</span>
      </p>
      <ol aria-label={t.route.countries} class="divide-y divide-line rounded-lg border border-line">
        <For each={chosen(props.draft)} keyed={(c) => c.geo}>
          {(c, i) => (
            <li class="flex items-baseline gap-2 px-3 py-2">
              <Show when={!props.draft.fastest}>
                <span class="w-5 shrink-0 text-xs tabular-nums text-muted">{i() + 1}.</span>
              </Show>
              <span class="min-w-0 break-words font-medium">{fmt.country(c().geo)}</span>
              <Show when={c().providers.length < (props.offer.get(c().geo)?.length ?? 0)}>
                <span class="text-xs text-muted">{c().providers.join(", ")}</span>
              </Show>
              <span class="ml-auto text-xs tabular-nums text-muted">{latency(c(), props.exits, t, fmt.milliseconds)}</span>
            </li>
          )}
        </For>
      </ol>
      <p class="text-xs text-muted">{props.draft.strict ? `${t.route.strict}. ${t.route.strictHelp}` : t.route.fallback}</p>
      <p class="text-xs text-muted">{t.route.readOnly}</p>
    </>
  );
}
