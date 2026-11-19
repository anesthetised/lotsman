// The operator console's own clients. Kept out of client.ts so that only the
// console's chunks carry them, not the user page.

import { createClient } from "@connectrpc/connect";
import { ConnectionService } from "../gen/lotsman/v1/connection_pb";
import { UserService } from "../gen/lotsman/v1/user_pb";
import { transport } from "./client";

export const admin = {
  users: createClient(UserService, transport),
  connections: createClient(ConnectionService, transport),
};
