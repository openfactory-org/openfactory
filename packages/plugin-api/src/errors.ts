// Errors that cross the plugin RPC boundary. A thrown OfError reaches the
// caller as a plain Error whose message is "<code>: <detail>", which the control
// plane rebuilds with toOfError. An optional method that a plugin does not
// support throws operation_not_supported.

import { OfError, toOfError } from "@openfactory/core";
import { ApiError } from "./http";

export function notSupported(what: string): OfError {
  return new OfError("operation_not_supported", what);
}

export function isNotSupported(e: unknown): boolean {
  return toOfError(e).error.code === "operation_not_supported";
}

/** The OfError for an outside API failure. */
export function fromApiError(e: ApiError): OfError {
  const detail = e.message;
  switch (true) {
    case e.status === 401:
      return new OfError("external_authentication_required", detail, { cause: e });
    case e.status === 403:
      return new OfError("not_authorized", detail, { cause: e });
    case e.status === 404:
      return new OfError("resource_not_found", detail, { cause: e });
    case e.status === 409:
      return new OfError("conflict", detail, { cause: e });
    case e.status === 400 || e.status === 422:
      return new OfError("invalid_request", detail, { cause: e });
    case e.temporary:
      return new OfError("resource_unavailable", detail, { cause: e });
    default:
      return new OfError("internal_error", detail, { cause: e });
  }
}

/** Maps any thrown value to an OfError so RPC callers see a stable code. */
export function toPluginError(e: unknown): OfError {
  if (e instanceof OfError) return e;
  if (e instanceof ApiError) return fromApiError(e);
  const { error, known } = toOfError(e);
  if (known) return error;
  const detail = e instanceof Error ? e.message : String(e);
  return new OfError("internal_error", detail, { cause: e });
}

/** Runs one RPC method body and rethrows failures as OfError. */
export async function rpc<T>(fn: () => Promise<T> | T): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    throw toPluginError(e);
  }
}
