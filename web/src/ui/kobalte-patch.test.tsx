// Guards patches/@kobalte__core@2.0.0-alpha.4.patch (see web/README.md).
// Unpatched, DismissableLayer never registers its layer when the content sits
// directly in the portal, so Escape and outside clicks do nothing. Our Dialog
// wraps the content in a div, which happens to hide the bug; bare Kobalte
// shows it. Plain render, not the testing library's, whose extra flush()
// after mounting hides it too.

import { Dialog } from "@kobalte/core/dialog";
import { fireEvent, screen, waitFor } from "@solidjs/testing-library";
import { render } from "@solidjs/web";
import { expect, onTestFinished, test } from "vitest";

test("a bare Kobalte dialog closes on Escape", async () => {
  const container = document.body.appendChild(document.createElement("div"));
  const dispose = render(
    () => (
      <Dialog>
        <Dialog.Trigger>Open</Dialog.Trigger>
        <Dialog.Portal>
          <Dialog.Content>
            <Dialog.Title>Title</Dialog.Title>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog>
    ),
    container,
  );
  onTestFinished(() => {
    dispose();
    container.remove();
  });

  screen.getByRole("button", { name: "Open" }).click();
  await screen.findByRole("dialog", { name: "Title" });
  fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});
