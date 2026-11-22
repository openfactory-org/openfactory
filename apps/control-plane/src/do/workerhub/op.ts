// Result helpers for TeamDO.api calls, kept here (not imported from
// src/do/team/index) so that the API modules do not import the TeamDO class
// and its route table (an import cycle through src/http/routes.ts).

/** An error result of TeamDO.api(). */
export interface OpErrorResult {
  $error: { code: string; detail: string };
}

export function isOpError(v: unknown): v is OpErrorResult {
  return typeof v === "object" && v !== null && typeof (v as OpErrorResult).$error === "object";
}
