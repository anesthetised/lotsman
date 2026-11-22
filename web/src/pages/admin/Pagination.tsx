import { Show } from "solid-js";
import { useConsoleI18n } from "../../i18n/console";
import { buttonClass } from "../../ui/button";

export const pageSize = 25;

export function Pagination(props: { page: number; hasMore: boolean; onPage: (page: number) => void }) {
  const { c, fmt } = useConsoleI18n();
  return (
    <Show when={props.page > 0 || props.hasMore}>
      <nav aria-label={c.pagination.label} class="flex flex-wrap items-center justify-end gap-2">
        <button type="button" class={buttonClass()} disabled={props.page === 0} onClick={() => props.onPage(props.page - 1)}>
          {c.pagination.previous}
        </button>
        <span class="px-1 text-muted tabular-nums">{c.pagination.page(fmt.count(props.page + 1))}</span>
        <button type="button" class={buttonClass()} disabled={!props.hasMore} onClick={() => props.onPage(props.page + 1)}>
          {c.pagination.next}
        </button>
      </nav>
    </Show>
  );
}
