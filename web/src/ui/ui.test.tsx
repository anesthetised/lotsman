import { fireEvent, render, screen, waitFor } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import { describe, expect, test, vi } from "vitest";
import { i18n, I18nContext } from "../i18n";
import { AvailabilityBar, availabilityLevel } from "./AvailabilityBar";
import { ChoiceMenu } from "./ChoiceMenu";
import { Dialog } from "./Dialog";
import { QrCode, qrCode } from "./QrCode";
import { sparklinePath } from "./Sparkline";

describe("Dialog", () => {
  test("opens from its trigger and closes on Escape, returning focus", async () => {
    render(() => (
      <Dialog trigger="Details" title="Upstream lv-1" description="Peers">
        <p>body</p>
      </Dialog>
    ));
    const trigger = screen.getByRole("button", { name: "Details" });
    fireEvent.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Upstream lv-1" });
    expect(dialog.textContent).toContain("body");

    fireEvent.keyDown(document.activeElement!, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  test("labels the close button in the page language", async () => {
    render(() => (
      <I18nContext value={i18n("ru")}>
        <Dialog trigger="Open" title="T" />
      </I18nContext>
    ));
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(await screen.findByRole("button", { name: "Закрыть" })).toBeTruthy();
  });
});

describe("ChoiceMenu", () => {
  test("shows the choices as radio items and reports the pick", async () => {
    const onChange = vi.fn();
    const [value] = createSignal<"1" | "5">("1");
    render(() => (
      <ChoiceMenu
        label="Window"
        value={value()}
        choices={[
          { value: "1", label: "1 min" },
          { value: "5", label: "5 min" },
        ]}
        onChange={onChange}
      />
    ));
    fireEvent.keyDown(screen.getByRole("button", { name: "Window" }), { key: "Enter" });
    const items = await screen.findAllByRole("menuitemradio");
    expect(items.map((i) => [i.textContent, i.getAttribute("aria-checked")])).toEqual([
      ["1 min", "true"],
      ["5 min", "false"],
    ]);
    fireEvent.keyDown(items[1]!, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("5");
    // Closing waits for Kobalte's presence animation, which jsdom never ends;
    // the Playwright smoke test covers it in a real browser.
  });
});

test("sparklinePath scales values into the box", () => {
  expect(sparklinePath([])).toBe("");
  expect(sparklinePath([0, 5, 10], 100, 20)).toBe("M0.0,20.0L50.0,10.0L100.0,0.0");
});

test("availability levels and segments", () => {
  expect([1, 0.995, 0.95, 0.5, undefined].map(availabilityLevel)).toEqual(["ok", "ok", "warn", "bad", "none"]);
  render(() => <AvailabilityBar ratios={[1, 0.5, undefined]} label="lv-1: 50 %" />);
  const bar = screen.getByRole("img", { name: "lv-1: 50 %" });
  expect([...bar.querySelectorAll("rect")].map((r) => r.getAttribute("class"))).toEqual(["fill-ok", "fill-bad", "fill-empty"]);
});

test("QR codes are inline SVG paths, and too long a text says so", () => {
  const code = qrCode("[Interface]\nPrivateKey = x\n");
  expect(code?.size).toBe(25); // version 2: 26 bytes
  expect(code?.path).toMatch(/^M[\d ]+L/);
  expect(qrCode("x".repeat(3000))).toBeUndefined();

  render(() => <QrCode text="hello" label="QR of hello" tooLong="too long" />);
  const svg = screen.getByRole("img", { name: "QR of hello" });
  expect(svg.tagName).toBe("svg");
  expect(svg.querySelector("path")?.getAttribute("d")).toBe(qrCode("hello")?.path);
  expect(document.querySelector("img")).toBeNull();
});

test("a QR code that does not fit shows the fallback", () => {
  render(() => <QrCode text={"x".repeat(3000)} label="QR" tooLong="too long" />);
  expect(screen.queryByRole("img")).toBeNull();
  expect(screen.getByText("too long")).toBeTruthy();
});
