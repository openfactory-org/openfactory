// Node-only helpers (CLI): read a definition tree from disk. Port of
// definition.LoadDir. Import from "@openfactory/definition/node"; the Worker
// bundle never loads this module.

import { lstatSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { OfError } from './errors.ts';

const MAX_FILE = 4 << 20;

/**
 * loadDir reads a definition tree, returning slash-separated paths relative
 * to root. Hidden files and directories and node_modules are skipped.
 */
export function loadDir(root: string): Record<string, string> {
  const info = statSync(root);
  if (!info.isDirectory()) throw new OfError('invalid_request', `${root} is not a directory`);
  const files: Record<string, string> = {};
  const walk = (dir: string, rel: string): void => {
    const entries = readdirSync(dir).sort();
    for (const name of entries) {
      const full = join(dir, name);
      const p = rel === '' ? name : rel + '/' + name;
      const st = lstatSync(full);
      if (st.isDirectory()) {
        if (name.startsWith('.') || name === 'node_modules') continue;
        walk(full, p);
        continue;
      }
      if (!st.isFile() || name.startsWith('.')) continue;
      if (st.size > MAX_FILE) throw new OfError('invalid_request', `${full} is larger than ${MAX_FILE} bytes`);
      files[p] = readFileSync(full, 'utf8');
    }
  };
  walk(root, '');
  return files;
}
