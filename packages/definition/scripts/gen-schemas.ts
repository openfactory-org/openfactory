// Writes the v1 definition JSON Schemas to schemas/definition/v1/ from the
// generator in src/schemagen.ts (port of internal/definition/schemagen_test.go).
//
//   node packages/definition/scripts/gen-schemas.ts           # rewrite
//   node packages/definition/scripts/gen-schemas.ts --check   # fail when stale

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { generatedSchemas, goJSONIndent } from '../src/schemagen.ts';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
const dir = join(root, 'schemas', 'definition', 'v1');
const check = process.argv.includes('--check');
let stale = 0;
for (const [name, s] of Object.entries(generatedSchemas())) {
  const file = join(dir, name + '.schema.json');
  const want = goJSONIndent(s);
  let got: string;
  try {
    got = readFileSync(file, 'utf8');
  } catch {
    got = '';
  }
  if (got === want) continue;
  if (check) {
    console.error(`${file} is out of date; run npm run gen:schemas`);
    stale++;
  } else {
    writeFileSync(file, want);
    console.log(`wrote ${file}`);
  }
}
if (stale > 0) process.exit(1);
