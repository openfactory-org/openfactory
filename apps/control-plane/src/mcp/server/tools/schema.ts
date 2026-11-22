// Factory MCP tools over the definition schemas: read a schema, validate a
// definition tree.
import { invalid, obj, s, str, tool, type Json } from "./common";

const APPLIES_TO: Record<string, string> = {
  "agent.schema.json": "agents/<name>/agent.md frontmatter",
  "automation.schema.json": "automations/<name>/automation.md frontmatter",
  "benchmark_suite.schema.json": "benchmarks/<suite>/suite.yaml",
  "benchmark_suite_task.schema.json": "benchmarks/<suite>/tasks/<task>.yaml",
  "common.schema.json": "shared definitions",
  "factory.schema.json": "factory.yaml",
  "machine.schema.json": "machines/<name>.yaml",
  "scorer.schema.json": "scorers/<name>/scorer.md frontmatter",
  "webhook.schema.json": "webhooks/<name>.yaml",
};

export const getFactoryFileSchema = tool(
  "get_factory_file_schema",
  "Returns the JSON Schema documents for factory definition files, as a catalog or one document at a time.",
  true,
  obj({
    document: str("schema document name such as factory.schema.json or agent.schema.json; omit for the catalog"),
    version: str("schema version; defaults to the current version"),
  }),
  async (c, a) => {
    let version = s(a, "version");
    if (version === "") {
      const v = await c.backend.api<{ current_version: string }>(c.env, c.principal, "GET", "/factory-files/schemas");
      version = v.current_version;
    }
    const doc = s(a, "document");
    if (doc !== "") {
      const schema = await c.backend.api(
        c.env,
        c.principal,
        "GET",
        `/factory-files/schemas/${encodeURIComponent(version)}/${encodeURIComponent(doc)}`,
      );
      return { version, document: doc, schema };
    }
    const docs = await c.backend.api<Json>(
      c.env,
      c.principal,
      "GET",
      `/factory-files/schemas/${encodeURIComponent(version)}`,
    );
    return {
      applies_to: APPLIES_TO,
      documents: Object.keys(docs ?? {}).sort(),
      url: `${c.publicURL}/api/v1/factory-files/schemas/${version}/{document}`,
      version,
    };
  },
);

export const validateFactoryFiles = tool(
  "validate_factory_files",
  "Validates a complete factory definition tree without saving or applying it.",
  true,
  obj(
    {
      files: {
        type: "object",
        description:
          "the complete definition tree: path relative to the factory root (factory.yaml, agents/<name>/agent.md, ...) mapped to file content",
        additionalProperties: { type: "string" },
      },
    },
    ["files"],
  ),
  async (c, a) => {
    const files = (typeof a.files === "object" && a.files !== null ? a.files : {}) as Record<string, string>;
    if (Object.keys(files).length === 0) throw invalid("files is required");
    return c.backend.validateFiles(c.env, files);
  },
);
