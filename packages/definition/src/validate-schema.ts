// Structural validation of one definition file against its JSON Schema, with
// @cfworker/json-schema (no eval). Port of definition.ValidateSchema. The
// compiler performs the complete semantic validation; the Worker does not
// need this module. The CLI and tests use it.

import { Validator } from '@cfworker/json-schema';
import { SCHEMA_BASE_URL, SCHEMA_NAMES, schema, schemaForPath } from './schema.ts';
import { parseYAML, splitFrontmatter, toValue } from './yamlnode.ts';

export interface SchemaError {
  /** JSON pointer of the value, "" for the document. */
  instance: string;
  keyword: string;
  message: string;
}

export interface SchemaResult {
  valid: boolean;
  errors: SchemaError[];
}

let validators: Map<string, Validator> | null = null;

function validatorFor(name: string): Validator {
  if (validators === null) {
    validators = new Map();
    const docs = SCHEMA_NAMES.map((n) => [n, JSON.parse(schema(n)!) as object] as const);
    for (const [n, doc] of docs) {
      const v = new Validator(doc as never, '2020-12', false);
      for (const [m, other] of docs) if (m !== n) v.addSchema(other as never, SCHEMA_BASE_URL + m + '.schema.json');
      validators.set(n, v);
    }
  }
  return validators.get(name)!;
}

/**
 * validateSchema validates one definition file (Markdown through its
 * frontmatter). Files without a schema are valid.
 */
export function validateSchema(path: string, content: string): SchemaResult {
  const name = schemaForPath(path);
  if (name === '') return { valid: true, errors: [] };
  let src = content;
  if (path.endsWith('.md')) {
    const fm = splitFrontmatter(content);
    if (fm.error !== null) return { valid: false, errors: [{ instance: '', keyword: 'yaml', message: fm.error }] };
    src = fm.hasFM ? fm.fm : '';
  }
  const r = parseYAML(src);
  if (r.error !== null) return { valid: false, errors: [{ instance: '', keyword: 'yaml', message: 'invalid YAML: ' + r.error.message }] };
  const inst = r.root === null ? {} : (toValue(r.root) ?? {});
  const res = validatorFor(name).validate(inst);
  return {
    valid: res.valid,
    errors: res.errors.map((e) => ({ instance: e.instanceLocation.replace(/^#/, ''), keyword: e.keyword, message: e.error })),
  };
}
