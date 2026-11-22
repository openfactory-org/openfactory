// Minimal Node declarations for tests that read package files. The package
// itself compiles against Workers types only (no node: imports in src/).
declare module "node:fs" {
  export function readFileSync(path: string | URL, encoding: "utf8"): string;
  export function readdirSync(path: string | URL): string[];
}

interface ImportMeta {
  readonly url: string;
}
