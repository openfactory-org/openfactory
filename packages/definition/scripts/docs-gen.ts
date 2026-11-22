// Writes the field tables of docs/definition-reference.md from the v1 JSON
// Schemas, so that the page and the schemas cannot drift. Port of
// scripts/definition-docs.
//
//   node packages/definition/scripts/docs-gen.ts                 # rewrite the generated sections
//   node packages/definition/scripts/docs-gen.ts --check         # fail when a section is stale
//   node packages/definition/scripts/docs-gen.ts --file <path>   # another Markdown file
//   node packages/definition/scripts/docs-gen.ts --out <path>    # write the result elsewhere
//
// Each table sits between <!-- BEGIN GENERATED: <id> (make docs-gen) --> and
// <!-- END GENERATED: <id> -->. Text outside the markers stays as it is.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { render } from '../src/docsgen.ts';
import { SCHEMA_NAMES, schema } from '../src/schema.ts';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
const args = process.argv.slice(2);
const opt = (name: string): string | undefined => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const file = opt('--file') ?? join(root, 'docs', 'definition-reference.md');
const out = opt('--out') ?? file;
const check = args.includes('--check');

const schemas: Record<string, Record<string, unknown>> = {};
for (const n of SCHEMA_NAMES) schemas[n] = JSON.parse(schema(n)!) as Record<string, unknown>;

const old = readFileSync(file, 'utf8');
let next: string;
try {
  next = render(old, schemas);
} catch (e) {
  console.error(`docs-gen: ${file}: ${(e as Error).message}`);
  process.exit(1);
}
if (old === next && out === file) process.exit(0);
if (check) {
  console.error(`docs-gen: ${file} is out of date with the JSON Schemas; run npm run gen:docs`);
  process.exit(1);
}
writeFileSync(out, next);
console.log(`wrote ${out}`);
