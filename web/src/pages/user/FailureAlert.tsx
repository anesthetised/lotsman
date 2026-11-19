import { Show } from "solid-js";
import { useI18n } from "../../i18n";
import { failureText, type Failure } from "./errors";

/** A failed call, in words, announced as it appears; nothing without one. */
export function FailureAlert(props: { failure: Failure | undefined }) {
  const { t } = useI18n();
  return (
    <Show when={props.failure}>
      {(failure) => (
        <p role="alert" class="rounded-md border border-bad p-3">
          {failureText(t, failure())}
        </p>
      )}
    </Show>
  );
}
