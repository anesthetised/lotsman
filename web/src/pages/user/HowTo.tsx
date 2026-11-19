// Short, generic instructions for the official Amnezia app: no provider or server names.

import { useI18n } from "../../i18n";
import { useUser } from "./context";

export function HowTo() {
  const { t } = useI18n();
  const ctx = useUser();
  const steps = () => [t.user.howTo.step1, t.user.howTo.step2, t.user.howTo.step3, t.user.howTo.step4, t.user.howTo.step5];
  return (
    <details open={ctx.user().devices.length === 0} class="rounded-xl border border-line bg-surface p-4">
      <summary class="flex min-h-11 cursor-pointer items-center text-lg font-medium">{t.user.howTo.title}</summary>
      <p class="mt-2 text-sm">{t.user.howTo.intro}</p>
      <ol class="mt-2 list-decimal space-y-2 pl-5 text-sm">
        {steps().map((step) => (
          <li>{step}</li>
        ))}
      </ol>
    </details>
  );
}
