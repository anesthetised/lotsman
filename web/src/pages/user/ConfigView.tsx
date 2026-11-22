// One device's config: the QR code, the file and the text, handed over in the
// way that suits the device the user is holding.

import { createMemo, createSignal, Errored, Loading, Show } from "solid-js";
import { useI18n } from "../../i18n";
import { downloadConfig } from "../../ui/downloadConfig";
import { QrCode } from "../../ui/QrCode";
import { confFilename } from "./config";
import { useUser } from "./context";
import { failureOf, failureText } from "./errors";
import { touchButton } from "./touch";

/** The config of a device: the one just issued, else fetched on demand (it holds the private key). */
export function PeerConfig(props: { peerId: string; device: string }) {
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
        <ConfigView device={props.device} config={config()} />
      </Loading>
    </Errored>
  );
}

export function ConfigView(props: { device: string; config: string }) {
  const { t } = useI18n();
  const [copy, setCopy] = createSignal<"idle" | "copied" | "failed">("idle");
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
      <QrCode
        text={props.config}
        label={t.user.config.qrLabel(props.device)}
        tooLong={t.user.config.qrTooLarge}
        class="mx-auto block max-w-[24rem]"
      />
      <p class="text-sm text-muted">{t.user.config.qrHint}</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class={touchButton("primary")} onClick={() => downloadConfig(props.config, confFilename(props.device))}>
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
