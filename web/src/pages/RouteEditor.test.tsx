// The route editor in a DOM: its controls, what it sends, and how it reads
// without the right to change the route.

import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@solidjs/testing-library";
import { createSignal, flush } from "solid-js";
import { afterEach, expect, test, vi } from "vitest";
import { RoutingBlock, UpstreamState, UpstreamStatusSchema } from "../gen/lotsman/v1/status_pb";
import { type Route, RouteSchema } from "../gen/lotsman/v1/user_pb";
import { RouteEditor } from "./RouteEditor";

const exit = (name: string, geo: string, provider: string, state = UpstreamState.UP, latencyMs = 50) =>
  create(UpstreamStatusSchema, { name, geo, provider, state, latency: state === UpstreamState.DOWN ? undefined : durationFromMs(latencyMs) });

const exits = [
  exit("lv-1", "LV", "provider-a", UpstreamState.UP, 38),
  exit("lv-2", "LV", "provider-b", UpstreamState.UP, 52),
  exit("nl-1", "NL", "provider-c", UpstreamState.DEGRADED, 140),
  exit("de-1", "DE", "provider-a", UpstreamState.DOWN),
];

afterEach(cleanup);

const ordered = create(RouteSchema, { fastest: false, strict: false, countries: [{ geo: "LV" }, { geo: "NL" }] });

/** The countries in the list's order, by their switches. */
const order = () => screen.getAllByRole("switch", { name: /^Use / }).map((s) => s.getAttribute("aria-label")!.slice("Use ".length));
/** The countries' rows as text. */
const countries = () => within(screen.getByRole("list", { name: "Countries" })).getAllByRole("listitem").map((li) => li.textContent);

test("reorders from the keyboard, says where the country went, and applies what changed", async () => {
  const apply = vi.fn(async (_: Route) => undefined);
  render(() => <RouteEditor route={ordered} exits={exits} providers apply={apply} />);
  expect(screen.getByRole("button", { name: "In order" }).getAttribute("aria-pressed")).toBe("true");
  // The chosen countries in order, then the others; each with its latency now.
  expect(order()).toEqual(["Latvia", "Netherlands", "Germany"]);
  expect(countries()[0]).toContain("38 ms");
  expect(countries()[2]).toContain("unavailable");
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);

  const latvia = screen.getByRole("button", { name: "Latvia, 1 of 2" });
  expect(latvia.getAttribute("aria-describedby")).toBe(screen.getByText("Reorder by dragging, or with the arrow keys.").id);
  latvia.focus();
  fireEvent.keyDown(latvia, { key: "ArrowDown" });
  flush();
  const moved = screen.getByRole("button", { name: "Latvia, 2 of 2" });
  expect(document.activeElement).toBe(moved);
  expect(screen.getByText("Latvia moved to 2 of 2")).toBeTruthy();
  fireEvent.keyDown(moved, { key: "ArrowDown" }); // already last
  flush();
  expect(screen.getByRole("button", { name: "Netherlands, 1 of 2" })).toBeTruthy();

  fireEvent.click(screen.getByRole("switch", { name: "Only the selected countries" }));
  fireEvent.click(screen.getByRole("button", { name: "provider-b" }));
  flush();
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(apply).toHaveBeenCalledOnce());
  expect(apply.mock.calls[0]![0]).toEqual(
    create(RouteSchema, { fastest: false, strict: true, countries: [{ geo: "NL" }, { geo: "LV", providers: ["provider-a"] }] }),
  );
});

test("switches countries and providers, including the last one, and Cancel goes back", () => {
  render(() => <RouteEditor route={ordered} exits={exits} providers apply={async () => undefined} />);
  const use = (name: string) => screen.getByRole("switch", { name: `Use ${name}` });
  expect(use("Germany").getAttribute("aria-checked")).toBe("false");
  fireEvent.click(use("Germany"));
  flush();
  expect(screen.getByRole("button", { name: "Germany, 3 of 3" })).toBeTruthy();

  fireEvent.click(use("Latvia"));
  fireEvent.click(use("Netherlands"));
  flush();
  const providers = within(screen.getByRole("group", { name: "Providers in Germany" }));
  const unavailable = providers.getByRole("button", { name: /provider-a.*unavailable/ });
  expect((unavailable as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(unavailable);
  flush();
  expect(use("Germany").getAttribute("aria-checked")).toBe("false");
  expect(unavailable.getAttribute("aria-pressed")).toBe("false");
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByRole("status").textContent).toBe("Choose at least one exit to save the route.");
  fireEvent.click(unavailable);
  flush();
  expect(use("Germany").getAttribute("aria-checked")).toBe("true");

  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  flush();
  expect(use("Germany").getAttribute("aria-checked")).toBe("false");
  expect(use("Latvia").getAttribute("aria-checked")).toBe("true");
});

test("in the fastest mode there is no order; a strict route with nothing available warns", () => {
  const germany = create(RouteSchema, { fastest: true, strict: true, countries: [{ geo: "DE" }] });
  render(() => <RouteEditor route={germany} exits={exits} providers apply={async () => undefined} />);
  expect(screen.getByRole("button", { name: "Fastest" }).getAttribute("aria-pressed")).toBe("true");
  expect(screen.queryByRole("button", { name: /, 1 of/ })).toBeNull();
  expect(screen.getByRole("alert").textContent).toBe(
    "This route will block traffic while the selected exits are unavailable. Select another available country or provider.",
  );
  fireEvent.click(screen.getByRole("switch", { name: "Use Netherlands" }));
  flush();
  expect(screen.queryByRole("alert")).toBeNull();
});

test("live exclusion keeps the chosen provider and unsaved mode; another provider can still be saved", async () => {
  const saved = create(RouteSchema, { fastest: true, strict: false, countries: [{ geo: "LV", providers: ["provider-a"] }] });
  const apply = vi.fn(async (_: Route) => undefined);
  const [current, setCurrent] = createSignal(exits);
  render(() => <RouteEditor route={saved} exits={current()} providers apply={apply} />);
  fireEvent.click(screen.getByRole("button", { name: "In order" }));
  flush();
  setCurrent([{ ...exits[0]!, routingBlock: RoutingBlock.UNAVAILABLE }, ...exits.slice(1)]);
  flush();
  expect(screen.getByRole("status").textContent).toContain("another allowed exit");
  const providers = within(screen.getByRole("group", { name: "Providers in Latvia" }));
  expect(providers.getByRole("button", { name: /provider-a.*unavailable/ }).getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByRole("button", { name: "In order" }).getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(screen.getByRole("button", { name: "provider-b" }));
  flush();
  expect(screen.queryByRole("status")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(apply).toHaveBeenCalledOnce());
  expect(apply.mock.calls[0]![0].countries).toEqual([{ ...saved.countries[0], providers: [] }]);
});

test("a failure stays in words; a new stored route resets the draft", async () => {
  const [route, setRoute] = createSignal<Route>(ordered);
  render(() => <RouteEditor route={route()} exits={exits} providers apply={async () => () => "Not accepted: route: no exit in country FI"} />);
  fireEvent.click(screen.getByRole("button", { name: "Fastest" }));
  flush();
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Not accepted: route: no exit in country FI");

  setRoute(create(RouteSchema, { fastest: false, strict: true, countries: [{ geo: "NL" }] }));
  flush();
  expect(screen.getByRole("button", { name: "In order" }).getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByRole("switch", { name: "Use Netherlands" }).getAttribute("aria-checked")).toBe("true");
  expect(screen.getByRole("switch", { name: "Use Latvia" }).getAttribute("aria-checked")).toBe("false");
});

test("read-only: the route in words, no controls", () => {
  render(() => <RouteEditor route={ordered} exits={exits} providers readOnly apply={async () => undefined} />);
  expect(screen.queryAllByRole("button")).toEqual([]);
  expect(screen.queryAllByRole("switch")).toEqual([]);
  expect(countries()).toEqual(["1.Latvia38 ms", "2.Netherlands140 ms"]);
  expect(screen.getByText("Only the operator changes this route.")).toBeTruthy();
});

test("without providers, no chips", () => {
  render(() => <RouteEditor route={ordered} exits={exits} providers={false} apply={async () => undefined} />);
  expect(screen.queryByRole("group", { name: /^Providers in/ })).toBeNull();
  expect(screen.queryByText("provider-a")).toBeNull();
});
