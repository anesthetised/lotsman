// Changing a device's profile: the daemon has no such call, so a device gets
// the new profile first and loses the old one second. The device keeps a
// working config whichever step fails: nothing is removed before the new
// peer exists.

import type { DeviceConfig } from "../../gen/lotsman/v1/user_pb";

export interface DeviceCalls {
  addDevice(req: { device: string; profile: string }): Promise<DeviceConfig>;
  removeDevice(req: { device: string; profile: string }): Promise<unknown>;
}

/** The change failed at `stage`. After "remove" the device has both profiles, and `config` is the new one. */
export class ProfileChangeError extends Error {
  constructor(
    readonly stage: "add" | "remove",
    readonly cause: unknown,
    readonly config?: DeviceConfig,
  ) {
    super(`changing the profile failed while trying to ${stage}`);
    this.name = "ProfileChangeError";
  }
}

/** Gives `device` the profile `to` and then takes `from` away; resolves with the new config. */
export async function changeProfile(calls: DeviceCalls, device: string, from: string, to: string): Promise<DeviceConfig> {
  let config: DeviceConfig;
  try {
    config = await calls.addDevice({ device, profile: to });
  } catch (e) {
    throw new ProfileChangeError("add", e);
  }
  try {
    await calls.removeDevice({ device, profile: from });
  } catch (e) {
    throw new ProfileChangeError("remove", e, config);
  }
  return config;
}
