// REST handler: GET /version (public, WS4).
import { BUILD } from "../config";
import { json, type Handler } from "../http/handler";

export const getVersion: Handler = async () => {
  const v: Record<string, string> = { version: BUILD.version || "dev" };
  if (BUILD.commit) v["commit"] = BUILD.commit;
  if (BUILD.date) v["date"] = BUILD.date;
  return json(v);
};
