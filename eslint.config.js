// ESLint flat config for the TypeScript workspace. The dashboard (web/) keeps
// its own setup until WS15 moves it here. Local rules go in tools/eslint-rules.
import js from "@eslint/js";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: [
      "**/node_modules/**",
      "**/dist/**",
      "**/.wrangler/**",
      "web/**",
      "internal/**",
      "cmd/**",
      "pkg/**",
      "deploy/**",
      "docs/**",
      "examples/**",
      "scripts/**",
      "bin/**",
      ".owner/**",
      ".claude/**",
      "packages/api-client/src/schema.d.ts",
      "**/src/generated/**",
      "**/worker-configuration.d.ts",
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.ts"],
    rules: {
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
      "@typescript-eslint/consistent-type-imports": "error",
      "no-restricted-syntax": [
        "error",
        {
          // DO rule 1: no await inside ctx.storage.transactionSync.
          selector: "CallExpression[callee.property.name='transactionSync'] AwaitExpression",
          message: "No await inside transactionSync: one synchronous transaction per request or alarm step.",
        },
        {
          selector: "CallExpression[callee.property.name='transactionSync'] > :function[async=true]",
          message: "transactionSync takes a synchronous callback.",
        },
        {
          selector: "CallExpression[callee.name='eval']",
          message: "Workers forbid eval.",
        },
      ],
    },
  },
  {
    files: ["tools/**/*.mjs", "*.js", "*.mjs", "**/scripts/**/*.mjs"],
    languageOptions: {
      globals: { process: "readonly", console: "readonly", URL: "readonly" },
    },
  },
);
