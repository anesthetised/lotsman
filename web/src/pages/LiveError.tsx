import { useI18n } from "../i18n";
import { buttonClass } from "../ui/button";

/** Shown when a live stream fails; Retry reopens it. */
export function LiveError(props: { error: unknown; retry: () => void }) {
  const { t } = useI18n();
  const message = () => (props.error instanceof Error ? props.error.message : String(props.error));
  return (
    <div role="alert" class="flex items-center gap-3 rounded-md border border-bad bg-surface p-3">
      <span>{t.common.liveError(message())}</span>
      <button type="button" class={buttonClass()} onClick={() => props.retry()}>
        {t.common.retry}
      </button>
    </div>
  );
}
