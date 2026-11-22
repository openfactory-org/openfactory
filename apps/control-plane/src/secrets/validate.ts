// Secret names, types and values. Port of internal/secrets/validate.go and
// the API checks of internal/api/platform.go. The AWS types
// (anthropic_bedrock_*, aws_ecr_credential) are removed (plan: no AWS).
import { OfError } from "@openfactory/core";

export const MAX_NAME_LENGTH = 128;
/** NAME=value plus the NUL byte must fit in 128 KiB. */
export const MAX_ENV_ENTRY = 128 << 10;
export const SECRET_TYPES = ["generic", "anthropic_api_key", "anthropic_oauth_token", "openai_api_key", "docker_registry"] as const;
export type SecretType = (typeof SECRET_TYPES)[number];
export const RESERVED_PREFIXES = ["OF_"];

const NAME_RE = /^[A-Z_][A-Z0-9_]*$/;
/** The looser name check of the REST API (internal/api secretNameRe). */
export const API_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

const bytes = (s: string) => new TextEncoder().encode(s).length;

export function isSecretType(t: string): t is SecretType {
  return (SECRET_TYPES as readonly string[]).includes(t);
}

/** ValidateName: UPPER_SNAKE_CASE, 1-128 characters, no reserved prefix. */
export function validateName(name: string): void {
  if (name === "") throw new OfError("invalid_request", "secret name is required");
  if (name.length > MAX_NAME_LENGTH) throw new OfError("invalid_request", `secret name exceeds ${MAX_NAME_LENGTH} characters`);
  if (!NAME_RE.test(name)) {
    throw new OfError(
      "invalid_request",
      `secret name ${JSON.stringify(name)} must use uppercase letters, digits and underscores and must not start with a digit (e.g. METABASE_API_KEY)`,
    );
  }
  for (const p of RESERVED_PREFIXES) {
    if (name.startsWith(p)) throw new OfError("invalid_request", `secret name ${JSON.stringify(name)} uses the reserved prefix ${p}`);
  }
}

function normalizeRegistry(r: string): string {
  let h = r.trim().toLowerCase();
  if (h.includes("://")) {
    try {
      h = new URL(h).host;
    } catch {
      h = "";
    }
    if (!h) throw new OfError("invalid_request", `invalid registry ${JSON.stringify(r)}`);
  }
  h = h.replace(/\/$/, "");
  if (h === "" || /[/ ]/.test(h)) throw new OfError("invalid_request", "registry must be a host name such as registry.example.com");
  return canonicalRegistry(h);
}

export function canonicalRegistry(h: string): string {
  return ["index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "docker.io"].includes(h) ? "docker.io" : h;
}

/** ParseDockerRegistry: strict JSON {registry, username, password}. */
export function parseDockerRegistry(value: string): { registry: string; username: string; password: string } {
  let v: Record<string, unknown>;
  try {
    v = JSON.parse(value) as Record<string, unknown>;
    if (typeof v !== "object" || v === null || Array.isArray(v)) throw new Error("not an object");
    for (const k of Object.keys(v)) if (!["registry", "username", "password"].includes(k)) throw new Error(`json: unknown field "${k}"`);
  } catch (e) {
    throw new OfError("invalid_request", `docker_registry must be JSON {registry, username, password}: ${(e as Error).message}`);
  }
  const s = (k: string) => (typeof v[k] === "string" ? (v[k] as string) : "");
  if (!s("registry") || !s("username") || !s("password")) {
    throw new OfError("invalid_request", "docker_registry requires registry, username and password");
  }
  return { registry: normalizeRegistry(s("registry")), username: s("username"), password: s("password") };
}

/** ValidateValue: UTF-8 text, no NUL, size limit, and the type's format. */
export function validateValue(name: string, type: string, value: string): void {
  if (value === "") throw new OfError("invalid_request", "secret value is required");
  if (value.includes("\0")) throw new OfError("invalid_request", "secret value must not contain NUL bytes");
  if (bytes(name) + 1 + bytes(value) + 1 > MAX_ENV_ENTRY) {
    throw new OfError(
      "invalid_request",
      `secret ${name} is too large: NAME=value must fit within ${MAX_ENV_ENTRY} bytes including the terminating null byte`,
    );
  }
  switch (type) {
    case "generic":
      return;
    case "anthropic_api_key":
    case "anthropic_oauth_token":
    case "openai_api_key":
      if (/\s/u.test(value)) throw new OfError("invalid_request", `${type} must be a single token without whitespace`);
      return;
    case "docker_registry":
      parseDockerRegistry(value);
      return;
  }
  throw new OfError("invalid_request", `unknown secret type ${JSON.stringify(type)}`);
}

/** API validateSecretValue (before the store's own checks). */
export function validateApiValue(name: string, value: string): void {
  if (value === "") throw new OfError("invalid_request", "value is required");
  if (bytes(name) + 1 + bytes(value) + 1 > MAX_ENV_ENTRY) {
    throw new OfError("invalid_request", `secret value too large: NAME=value must fit in ${MAX_ENV_ENTRY} bytes`);
  }
  if (value.includes("\0")) throw new OfError("invalid_request", "secret values cannot contain NUL bytes");
}

/** ImageHost: registry host of an image reference (Docker rules). */
export function imageHost(image: string): string {
  let i = image.trim();
  const at = i.indexOf("@");
  if (at >= 0) i = i.slice(0, at);
  const slash = i.indexOf("/");
  if (slash < 0) return "docker.io";
  const first = i.slice(0, slash);
  if (/[.:]/.test(first) || first === "localhost") return canonicalRegistry(first.toLowerCase());
  return "docker.io";
}
