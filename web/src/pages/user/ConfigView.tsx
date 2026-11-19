// One peer's config: the QR code, the file and the text, handed over in the
// way that suits the device the user is holding.

import { createMemo, createSignal, Errored, Loading, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { confFilename, saveAsFile } from "./config";
import { useUser } from "./context";
import { failureOf, failureText } from "./errors";
import { qrShape } from "./qr";
import { touchButton } from "./touch";

/** The config of a peer: the one just issued, else fetched on demand (it holds the private key). */
export function PeerConfig(props: { peerId: string; device: string; profile: string }) {
  const { t } = useI18n();
  const ctx = useUser();
  const config = createMemo(async () => {
    const known = ctx.configs.get(props.peerId);
    if (known !== undefined) return known;
    const text = (await ctx.self.getConfig({ peerId: props.peerId })).config;
    ctx.configs.set(props.peerId, text);
    return text;
  });
  return (
    <Errored
      fallback={(error, reset) => (
        <div role="alert" class="flex flex-wrap items-center gap-3 rounded-md border border-bad p-3 text-sm">
          <span>{failureText(t, failureOf(error()))}</span>
          <button type="button" class={touchButton()} onClick={() => reset()}>
            {t.common.retry}
          </button>
        </div>
      )}
    >
      <Loading fallback={<p class="text-sm text-muted">{t.user.config.loading}</p>}>
        <ConfigView device={props.device} profile={props.profile} config={config()} />
      </Loading>
    </Errored>
  );
}

export function ConfigView(props: { device: string; profile: string; config: string }) {
  const { t } = useI18n();
  const [copy, setCopy] = createSignal<"idle" | "copied" | "failed">("idle");
  const shape = createMemo(() => {
    try {
      return qrShape(props.config);
    } catch {
      return undefined; // more text than the largest QR code holds
    }
  });
  const copyText = async () => {
    try {
      await navigator.clipboard.writeText(props.config);
      setCopy("copied");
    } catch {
      setCopy("failed");
    }
  };
  return (
    <div class="space-y-3">
      <Show when={shape()} fallback={<p class="text-sm text-muted">{t.user.config.qrTooLarge}</p>}>
        {(qr) => (
          <svg
            role="img"
            aria-label={t.user.config.qrLabel(props.device, props.profile)}
            viewBox={`0 0 ${qr().size} ${qr().size}`}
            shape-rendering="crispEdges"
            class="mx-auto block aspect-square w-full max-w-[24rem] rounded-md bg-white"
          >
            <path d={qr().path} fill="black" />
          </svg>
        )}
      </Show>
      <p class="text-sm text-muted">{t.user.config.qrHint}</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class={touchButton("primary")} onClick={() => saveAsFile(props.config, confFilename(props.device, props.profile))}>
          {t.user.config.download}
        </button>
        <button type="button" class={touchButton()} onClick={copyText}>
          {t.user.config.copy}
        </button>
      </div>
      <p role="status" class="text-sm">
        <Show when={copy() !== "idle"}>{copy() === "copied" ? t.user.config.copied : t.user.config.copyFailed}</Show>
      </p>
      <p class="text-xs text-muted">{t.user.config.secret}</p>
    </div>
  );
}
