// The console's forms against the fake daemon: what they check themselves,
// and how they show what the daemon refuses.

import { fireEvent, render, screen, waitFor, within } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { expect, test } from "vitest";
import Profiles from "./Profiles";
import Users from "./Users";

/** Types into a field. A browser runs the next event in a later task; here the write needs a flush. */
const input = (el: HTMLElement, value: string) => {
  fireEvent.input(el, { target: { value } });
  flush();
};

test("the profile form checks the name and the tiers, suggests upstream labels, and saves", async () => {
  render(() => <Profiles />);
  fireEvent.click(await screen.findByRole("button", { name: "New profile" }));
  const dialog = within(await screen.findByRole("dialog", { name: "New profile" }));
  const name = dialog.getByLabelText("Name");
  const save = dialog.getByRole("button", { name: "Save" });

  // Nothing is flagged before the first try.
  expect(name.getAttribute("aria-invalid")).toBeNull();
  fireEvent.click(save);
  await waitFor(() => expect(name.getAttribute("aria-invalid")).toBe("true"));
  expect(dialog.getByText("Give the profile a name.").id).toBe(name.getAttribute("aria-describedby"));

  input(name, "EU");
  await dialog.findByText(/^Lowercase letters, digits and -, up to 32 characters/);
  input(name, "default");
  await dialog.findByText("A profile of this name exists. Edit that one instead.");

  input(name, "eu");
  await waitFor(() => expect(name.getAttribute("aria-invalid")).toBeNull());
  expect(dialog.getByText("No tiers: any upstream, lowest latency first.")).toBeTruthy();
  fireEvent.click(dialog.getByRole("button", { name: "Add tier" }));
  const geo = await dialog.findByLabelText("Tier 1 geo");
  const options = (field: HTMLElement) => [...(field as HTMLInputElement).list!.options].map((o) => o.value);
  // Every geo an upstream has, and once a geo is set only what fits it.
  expect(options(geo)).toEqual(["AL", "DE", "LV", "NL"]);
  input(geo, "FI");
  await dialog.findByText("lotsman profile add eu geo=FI");
  expect(options(dialog.getByLabelText("Tier 1 provider"))).toEqual([]);

  // A tier that matches nothing is flagged here, before the daemon refuses it.
  fireEvent.click(save);
  const problem = await dialog.findByText("Tier 1 matches no upstream. The fields suggest the values that do.");
  expect(geo.getAttribute("aria-invalid")).toBe("true");
  expect(geo.getAttribute("aria-describedby")).toBe(problem.id);
  expect(document.activeElement).toBe(geo);
  expect(dialog.queryByRole("alert")).toBeNull();

  input(geo, "LV");
  expect(options(dialog.getByLabelText("Tier 1 provider"))).toEqual(["provider-a"]);
  await waitFor(() => expect(geo.getAttribute("aria-invalid")).toBeNull());
  fireEvent.click(dialog.getByRole("button", { name: "Add tier" }));
  input(await dialog.findByLabelText("Tier 2 tag"), "eu");
  fireEvent.click(save);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const row = await screen.findByRole("row", { name: /^eu/ });
  expect(within(row).getByRole("list", { name: "Tiers of eu" }).textContent).toBe("1. geo=LV2. tag=eu");
  expect(row.textContent).not.toContain("Warning");
});

test("adding a device shows its config with a QR code, or the daemon's refusal", async () => {
  render(() => <Users />);
  fireEvent.click(await screen.findByRole("button", { name: "Add device" }));
  let dialog = within(await screen.findByRole("dialog", { name: "Add a device" }));
  input(dialog.getByLabelText("User"), "alice");
  input(dialog.getByLabelText("Device"), "phone");
  expect((dialog.getByLabelText("Profile") as HTMLSelectElement).value).toBe("default");
  fireEvent.click(dialog.getByRole("button", { name: "Add" }));
  expect((await dialog.findByRole("alert")).textContent).toBe("It did not work: alice/phone already has profile default");

  input(dialog.getByLabelText("Device"), "x".repeat(65));
  fireEvent.click(dialog.getByRole("button", { name: "Add" }));
  await waitFor(() => expect(dialog.getByRole("alert").textContent).toMatch(/at most 64 bytes/));

  input(dialog.getByLabelText("Device"), "tablet");
  fireEvent.change(dialog.getByLabelText("Profile"), { target: { value: "streaming" } });
  flush();
  fireEvent.click(dialog.getByRole("button", { name: "Add" }));
  dialog = within(await screen.findByRole("dialog", { name: "Config of alice/tablet · streaming" }));
  expect((dialog.getByLabelText("Config file") as HTMLTextAreaElement).value).toMatch(/^\[Interface\]\n/);
  const qr = dialog.getByRole("img", { name: "QR code of the config of alice/tablet · streaming" });
  expect(qr.querySelector("path")?.getAttribute("d")).toMatch(/^M/);
  expect(dialog.getByRole("button", { name: "Download alice-tablet.conf" })).toBeTruthy();
  // The list behind already has the device.
  await waitFor(() => expect(screen.getByRole("region", { name: "alice", hidden: true }).textContent).toContain("tablet"));
});
