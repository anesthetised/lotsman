// Profiles: what each client's traffic prefers, as ordered tiers. The daemon
// validates and stores them (ProfileService); its warnings about tiers that
// never apply show next to the profile they concern.

import { Code, ConnectError } from "@connectrpc/connect";
import { createMemo, createSignal, Errored, flush, For, Loading, refresh, Show } from "solid-js";
import { admin } from "../../api/admin";
import { groupWarnings, type ProfileWarning, suggestions, type Tier, type TierProblem, tierProblem, tierString, type UpstreamLabels } from "../../api/profiles";
import type { Profile } from "../../gen/lotsman/v1/profile_pb";
import { useI18n } from "../../i18n";
import { useConsoleI18n } from "../../i18n/console";
import type { ConsoleDict } from "../../i18n/console.en";
import { buttonClass } from "../../ui/button";
import { Dialog } from "../../ui/Dialog";
import { ArrowDownIcon, ArrowUpIcon, CloseIcon, WarningIcon } from "../../ui/icons";
import { LiveError } from "../LiveError";
import { ApiError } from "./ApiError";
import { Confirm } from "./Confirm";
import { moveTier, nameProblem, removeTier, type TierRow, tierRow, toTiers, updateTier } from "./tiers";

export default function Profiles() {
  const { t } = useI18n();
  return (
    <Errored fallback={(error, reset) => <LiveError error={error()} retry={reset} />}>
      <Loading fallback={<p class="text-muted">{t.common.connecting}</p>}>
        <ProfileList />
      </Loading>
    </Errored>
  );
}

function ProfileList() {
  const { t, c } = useConsoleI18n();
  const list = createMemo(() => admin.profiles.list({}));
  const warnings = createMemo(() => groupWarnings(list().warnings));
  const names = createMemo(() => list().profiles.map((p) => p.name));
  const upstreams = createMemo(() => list().upstreams);
  const changed = () => refresh(list);
  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between gap-4">
        <h1 class="text-lg font-semibold">{c.profiles.title}</h1>
        <ProfileForm existing={names()} upstreams={upstreams()} onSaved={changed} />
      </div>
      <p class="max-w-3xl text-muted">{c.profiles.intro}</p>

      <Show when={warnings().other.length > 0}>
        <ul class="space-y-1 rounded-md border border-warn p-3">
          <For each={warnings().other}>{(text) => <WarningLine text={text} />}</For>
        </ul>
      </Show>

      <Show when={list().profiles.length > 0} fallback={<p class="rounded-lg border border-line bg-surface p-4 text-muted">{c.profiles.empty}</p>}>
        <section class="overflow-x-auto rounded-lg border border-line bg-surface">
          <table class="w-full min-w-[40rem]">
            <thead class="text-left text-xs text-muted">
              <tr>
                <th scope="col" class="px-3 py-2 font-medium">{t.admin.name}</th>
                <th scope="col" class="px-3 py-2 font-medium">{c.profiles.tiers}</th>
                <th scope="col" class="px-3 py-2">
                  <span class="sr-only">{c.profiles.actions}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              <For each={list().profiles} keyed={(p) => p.name}>
                {(p) => (
                  <ProfileRow
                    profile={p()}
                    warnings={warnings().byProfile.get(p().name) ?? []}
                    existing={names()}
                    upstreams={upstreams()}
                    onChanged={changed}
                  />
                )}
              </For>
            </tbody>
          </table>
        </section>
      </Show>
    </div>
  );
}

function ProfileRow(props: {
  profile: Profile;
  warnings: readonly ProfileWarning[];
  existing: readonly string[];
  upstreams: readonly UpstreamLabels[];
  onChanged: () => void;
}) {
  const { c } = useConsoleI18n();
  const warned = (tier: number) => props.warnings.some((w) => w.tier === tier);
  const inUse = (e: unknown) => (ConnectError.from(e).code === Code.FailedPrecondition ? c.profiles.inUse(props.profile.name) : undefined);
  return (
    <tr class="border-t border-line align-top">
      <th scope="row" class="px-3 py-2 text-left font-medium">
        {props.profile.name}
      </th>
      <td class="px-3 py-2">
        <Show when={props.profile.prefer.length > 0} fallback={<span class="text-muted">{c.profiles.anyUpstream}</span>}>
          <ol aria-label={c.profiles.tiersOf(props.profile.name)} class="flex flex-wrap gap-1">
            {/* By position: a refreshed list brings equal tiers as new objects. */}
            <For each={props.profile.prefer} keyed={false}>
              {(m, i) => (
                <li class={["rounded border px-1.5 py-0.5 font-mono text-xs", warned(i + 1) ? "border-warn" : "border-line"]}>
                  <span class="text-muted">{i + 1}.</span> {tierString(m())}
                </li>
              )}
            </For>
          </ol>
        </Show>
        <Show when={props.warnings.length > 0}>
          <ul class="mt-2 space-y-1">
            <For each={props.warnings}>{(w) => <WarningLine text={warningText(c, w)} title={w.text} />}</For>
          </ul>
        </Show>
      </td>
      <td class="px-3 py-2">
        <div class="flex justify-end gap-2">
          <ProfileForm profile={props.profile} existing={props.existing} upstreams={props.upstreams} onSaved={props.onChanged} />
          <Confirm
            trigger={
              <>
                {c.profiles.delete}
                <span class="sr-only"> {props.profile.name}</span>
              </>
            }
            title={c.profiles.deleteTitle(props.profile.name)}
            description={c.profiles.deleteDescription}
            confirm={c.profiles.delete}
            action={async () => {
              try {
                await admin.profiles.delete({ name: props.profile.name });
              } catch (e) {
                // Someone else deleted it first: the list is stale, not the action wrong.
                if (ConnectError.from(e).code !== Code.NotFound) throw e;
              }
              props.onChanged();
            }}
            failure={inUse}
          />
        </div>
      </td>
    </tr>
  );
}

function warningText(c: ConsoleDict, w: ProfileWarning): string {
  switch (w.kind) {
    case "unmatched":
      return c.profiles.unmatched(w.tier, w.match);
    case "shadowed":
      return c.profiles.shadowed(w.tier, w.match, w.by);
    default:
      return w.text;
  }
}

function WarningLine(props: { text: string; title?: string }) {
  const { c } = useConsoleI18n();
  return (
    <li class="flex items-start gap-1.5 text-sm" title={props.title}>
      <span class="text-warn">
        <WarningIcon />
      </span>
      <span>
        <span class="sr-only">{c.profiles.warning}: </span>
        {props.text}
      </span>
    </li>
  );
}

interface ProfileFormProps {
  /** The profile to edit; a new one when unset. */
  profile?: Profile;
  existing: readonly string[];
  /** The running upstreams' labels: the tier fields suggest them, and a tier must match one. */
  upstreams: readonly UpstreamLabels[];
  onSaved: () => void;
}

/** Creates or edits a profile in a dialog. Editing keeps the name: Put by name replaces. */
function ProfileForm(props: ProfileFormProps) {
  const { t, c } = useConsoleI18n();
  const editing = () => props.profile !== undefined;
  const [open, setOpen] = createSignal(false);
  const [name, setName] = createSignal("");
  const [rows, setRows] = createSignal<TierRow[]>([]);
  const [submitted, setSubmitted] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const problem = createMemo(() => nameProblem(name(), props.existing, editing()));
  // The daemon refuses these too; checking here points at the tier at once.
  const tierProblems = createMemo(() => toTiers(rows()).map((t) => tierProblem(t, props.upstreams)));
  const id = () => (props.profile ? `profile-${props.profile.name}` : "profile-new");

  const openChange = (next: boolean) => {
    setOpen(next);
    if (!next) return;
    setName(props.profile?.name ?? "");
    setRows((props.profile?.prefer ?? []).map((m) => tierRow(m)));
    setSubmitted(false);
    setError(undefined);
  };
  // The rows render by position, so a move changes their values, not their
  // nodes: a moved focused node would lose focus, and the dialog's focus trap
  // would take it to the first control. Flush the change into the DOM, then
  // focus where the row went.
  const focus = (elementId: string) => {
    flush();
    document.getElementById(elementId)?.focus();
  };
  const move = (row: TierRow, by: number, index: number) => {
    setRows((r) => moveTier(r, row.key, by));
    // Follow the row; at either end its other arrow is the one still enabled.
    const to = index + by;
    const edge = to === 0 ? "down" : to === rows().length - 1 ? "up" : by < 0 ? "up" : "down";
    focus(`${id()}-tier-${to}-${edge}`);
  };
  const remove = (row: TierRow) => {
    setRows((r) => removeTier(r, row.key));
    focus(`${id()}-add`);
  };
  const add = () => {
    const index = rows().length;
    setRows((r) => [...r, tierRow()]);
    focus(`${id()}-tier-${index}-geo`);
  };
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setError(undefined);
    if (problem()) {
      document.getElementById(`${id()}-name`)?.focus();
      return;
    }
    const bad = tierProblems().findIndex((p) => p !== undefined);
    if (bad >= 0) {
      document.getElementById(`${id()}-tier-${bad}-${invalidFields(rows()[bad]!, tierProblems()[bad]!)[0] ?? "geo"}`)?.focus();
      return;
    }
    setBusy(true);
    try {
      await admin.profiles.put({ profile: { name: name(), prefer: toTiers(rows()) } });
      setOpen(false);
      props.onSaved();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  const nameError = () => {
    const p = submitted() ? problem() : undefined;
    return p && c.profiles.nameProblem[p];
  };
  const tierError = (i: number) => {
    const p = submitted() ? tierProblems()[i] : undefined;
    if (!p) return undefined;
    if (p.kind === "unmatched") return c.profiles.tierUnmatched(i + 1);
    return p.field === "tag" ? c.profiles.tierTag(i + 1) : c.profiles.tierValue(i + 1, c.profiles.fields[p.field]);
  };
  const invalid = (i: number, field: keyof Tier) => {
    const p = submitted() ? tierProblems()[i] : undefined;
    return p !== undefined && invalidFields(rows()[i]!, p).includes(field);
  };
  const command = () => [editing() ? "lotsman profile set" : "lotsman profile add", name() || "…", ...toTiers(rows()).map(tierString)].join(" ");

  return (
    <Dialog
      open={open()}
      onOpenChange={openChange}
      wide
      variant={editing() ? "compact" : "primary"}
      trigger={
        editing() ? (
          <>
            {c.profiles.edit}
            <span class="sr-only"> {props.profile?.name}</span>
          </>
        ) : (
          c.profiles.create
        )
      }
      title={editing() ? c.profiles.editTitle(props.profile?.name ?? "") : c.profiles.createTitle}
      description={c.profiles.formDescription}
    >
      <form class="space-y-4 text-sm" onSubmit={submit} novalidate>
        <div class="space-y-1">
          <label for={`${id()}-name`} class="block font-medium">
            {t.admin.name}
          </label>
          <input
            id={`${id()}-name`}
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
            readonly={editing()}
            autocomplete="off"
            spellcheck={false}
            aria-invalid={nameError() ? "true" : undefined}
            aria-describedby={`${id()}-name-hint`}
            class="w-full max-w-xs rounded-md border border-line bg-bg px-3 py-1.5 font-mono read-only:text-muted aria-[invalid]:border-bad"
          />
          <p id={`${id()}-name-hint`} class={nameError() ? "text-bad" : "text-xs text-muted"}>
            {nameError() ?? (editing() ? c.profiles.nameFixed : c.profiles.nameHint)}
          </p>
        </div>

        <fieldset class="space-y-2">
          <legend class="font-medium">{c.profiles.tiers}</legend>
          <p class="text-xs text-muted">{c.profiles.tiersHint}</p>
          <Show when={rows().length > 0} fallback={<p class="text-muted">{c.profiles.noTiers}</p>}>
            <ol class="space-y-2">
              <For each={rows()} keyed={false}>
                {(row, i) => (
                  <li class="flex flex-wrap items-center gap-2 rounded-md border border-line p-2">
                    <span class="w-16 text-xs font-medium text-muted">{c.profiles.tierN(i + 1)}</span>
                    <For each={["geo", "provider", "tag"] as const}>
                      {(field) => (
                        <TierField
                          row={row()}
                          index={i}
                          field={field}
                          prefix={id()}
                          options={suggestions(row(), field, props.upstreams)}
                          invalid={invalid(i, field)}
                          describedBy={tierError(i) ? `${id()}-tier-${i}-problem` : undefined}
                          onInput={(v) => setRows((r) => updateTier(r, row().key, field, v))}
                        />
                      )}
                    </For>
                    <span class="w-20 truncate font-mono text-xs text-muted" aria-hidden="true">
                      = {tierString(row())}
                    </span>
                    <span class="ml-auto flex gap-1">
                      <button
                        type="button"
                        id={`${id()}-tier-${i}-up`}
                        class={buttonClass("ghost")}
                        disabled={i === 0}
                        aria-label={c.profiles.moveUp(i + 1)}
                        onClick={() => move(row(), -1, i)}
                      >
                        <ArrowUpIcon />
                      </button>
                      <button
                        type="button"
                        id={`${id()}-tier-${i}-down`}
                        class={buttonClass("ghost")}
                        disabled={i === rows().length - 1}
                        aria-label={c.profiles.moveDown(i + 1)}
                        onClick={() => move(row(), 1, i)}
                      >
                        <ArrowDownIcon />
                      </button>
                      <button type="button" class={buttonClass("ghost")} aria-label={c.profiles.removeTier(i + 1)} onClick={() => remove(row())}>
                        <CloseIcon />
                      </button>
                    </span>
                    <Show when={tierError(i)}>
                      {(text) => (
                        <p id={`${id()}-tier-${i}-problem`} class="w-full text-xs text-bad">
                          {text()}
                        </p>
                      )}
                    </Show>
                  </li>
                )}
              </For>
            </ol>
          </Show>
          <button type="button" id={`${id()}-add`} class={buttonClass()} onClick={add}>
            {c.profiles.addTier}
          </button>
        </fieldset>

        <p class="text-xs text-muted">
          {c.profiles.asCommand} <code class="font-mono text-fg">{command()}</code>
        </p>

        <ApiError error={error()} />
        <div class="flex justify-end gap-2">
          <button type="button" class={buttonClass()} onClick={() => setOpen(false)}>
            {t.admin.cancel}
          </button>
          <button type="submit" class={buttonClass("primary")} disabled={busy()}>
            {c.profiles.save}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

/** The fields a tier problem is about: the bad value, or every field set when nothing matches. */
function invalidFields(row: Tier, p: TierProblem): (keyof Tier)[] {
  if (p.kind === "value") return [p.field];
  const set = (["geo", "provider", "tag"] as const).filter((f) => row[f].trim() !== "");
  return set.length > 0 ? set : ["geo"];
}

interface TierFieldProps {
  row: TierRow;
  index: number;
  field: keyof Tier;
  prefix: string;
  /** Values that keep the tier matching an upstream, offered as the browser's suggestions. */
  options: readonly string[];
  invalid: boolean;
  describedBy?: string;
  onInput: (value: string) => void;
}

function TierField(props: TierFieldProps) {
  const { c } = useConsoleI18n();
  const id = () => `${props.prefix}-tier-${props.index}-${props.field}`;
  return (
    <>
      <input
        id={id()}
        value={props.row[props.field]}
        onInput={(e) => props.onInput(e.currentTarget.value)}
        placeholder={c.profiles.fields[props.field]}
        aria-label={c.profiles.fieldLabel(props.index + 1, c.profiles.fields[props.field])}
        aria-invalid={props.invalid ? "true" : undefined}
        aria-describedby={props.describedBy}
        list={`${id()}-options`}
        autocomplete="off"
        spellcheck={false}
        class="w-24 rounded-md border border-line bg-bg px-2 py-1 font-mono text-xs aria-[invalid]:border-bad"
      />
      <datalist id={`${id()}-options`}>
        <For each={props.options}>{(value) => <option value={value} />}</For>
      </datalist>
    </>
  );
}
