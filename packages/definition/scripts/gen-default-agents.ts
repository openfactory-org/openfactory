// Writes src/default-agents.generated.ts from defaults/agents/*/agent.md, so
// that the Worker bundle has the default agents as string constants.
//
//   node packages/definition/scripts/gen-default-agents.ts           # rewrite
//   node packages/definition/scripts/gen-default-agents.ts --check   # fail when stale

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const pkg = join(dirname(fileURLToPath(import.meta.url)), '..');
const names = ['foreman', 'triage', 'spec', 'implement', 'review'];

let s =
  '// Generated from packages/definition/defaults/agents/*/agent.md by\n' +
  '// scripts/gen-default-agents.ts. Do not edit by hand.\n\n' +
  '/** DEFAULT_AGENT_FILES maps agents/<name>/agent.md to its content. */\n' +
  'export const DEFAULT_AGENT_FILES: Readonly<Record<string, string>> = {\n';
for (const n of names) {
  const content = readFileSync(join(pkg, 'defaults', 'agents', n, 'agent.md'), 'utf8');
  s += `  ${JSON.stringify(`agents/${n}/agent.md`)}:\n    ${JSON.stringify(content)},\n`;
}
s += '};\n';

const out = join(pkg, 'src', 'default-agents.generated.ts');
const old = readFileSync(out, 'utf8');
if (process.argv.includes('--check')) {
  if (old !== s) {
    console.error(`${out} is out of date; run node packages/definition/scripts/gen-default-agents.ts`);
    process.exit(1);
  }
} else if (old !== s) {
  writeFileSync(out, s);
  console.log(`wrote ${out}`);
}
