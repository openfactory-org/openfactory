# openfactory

The command-line client of [Open Factory](https://github.com/openfactory-org/openfactory). Open Factory runs teams of AI coding agents ("factories") against your repositories.

## Install

```bash
npm install -g openfactory
```

Node.js 24 or later is required.

## Use

1. Set the control plane URL and a team API key:

   ```bash
   export OF_SERVER=https://factory.example.com
   export OF_API_KEY=of_...
   ```

2. Validate a definition tree locally:

   ```bash
   openfactory validate ./my-factory
   ```

3. Run `openfactory --help` for every command.

| Group | Commands |
|---|---|
| Client | `bootstrap`, `factory`, `run`, `work-item`, `inbox`, `secret`, `webhook`, `benchmark`, `mcp-registry`, `worker list` |
| Local | `validate`, `schema`, `init` |
| MCP | `mcp` (the Factory MCP over stdio) |
| Runtime | `runner`, `netguard`, `vm-init`, `worker` |

The runtime commands belong to the `openfactory-runtime` binary. The CLI passes them on when `openfactory-runtime` is on `PATH`.

## License

Apache-2.0
