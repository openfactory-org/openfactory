package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/pkg/sdk"
)

func newMCPRegistryCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp-registry",
		Short: "Manage the team MCP server registry",
		Long: `Register MCP servers once per team and reference them from agents with
mcpServers.<name>.managedId: <server name>. Credentials go in secrets and are
referenced as {{SECRET_NAME}}. A url server is reached through the
control-plane proxy by default, so its headers and OAuth tokens never enter
the sandbox. Use --direct only for a server that the control plane cannot
reach: its headers then go to the sandbox.`,
	}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List registered and built-in MCP servers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			list, err := c.MCPServers.List(ctx)
			if err != nil {
				return err
			}
			builtin, err := c.MCPServers.Builtin(ctx)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), map[string]any{"mcp_servers": list, "builtin": builtin})
			}
			rows := make([][]string, 0, len(list)+len(builtin))
			for _, m := range list {
				transport := m.URL
				if m.Command != "" {
					transport = strings.TrimSpace(m.Command + " " + strings.Join(m.Args, " "))
				}
				rows = append(rows, []string{m.Name, transport, mcpMode(m), ago(m.UpdatedAt)})
			}
			for _, b := range builtin {
				state := "integration not configured"
				if b.Configured {
					state = "available"
				}
				rows = append(rows, []string{b.ManagedID, "built-in", state, ""})
			}
			table(cmd.OutOrStdout(), []string{"NAME", "TRANSPORT", "MODE", "UPDATED"}, rows)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use: "get <name>", Short: "Show a registered MCP server", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			m, err := c.MCPServers.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), m)
		},
	})

	var (
		desc, url, command, clientID, clientSecret string
		margs, headers, env, scopes                []string
		direct, oauth                              bool
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Register an MCP server",
		Example: `  openfactory mcp-registry add sentry --url https://mcp.sentry.dev/mcp \
    --header 'Authorization=Bearer {{SENTRY_TOKEN}}'
  openfactory mcp-registry add private --url http://10.0.0.5:8080/mcp --direct
  openfactory mcp-registry add notion --url https://mcp.notion.com/mcp --oauth
  openfactory mcp-registry add docs --command npx --arg -y --arg @acme/docs-mcp`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			p := sdk.MCPServerParams{Name: &args[0], Args: margs}
			if desc != "" {
				p.Description = &desc
			}
			if url != "" {
				p.URL = &url
			}
			if command != "" {
				p.Command = &command
			}
			if p.Headers, err = keyValues("--header", headers); err != nil {
				return err
			}
			if p.Env, err = keyValues("--env", env); err != nil {
				return err
			}
			if direct {
				c := "direct"
				p.Connection = &c
			}
			if oauth || clientID != "" || len(scopes) > 0 {
				p.OAuth = &sdk.MCPOAuthConfig{Scopes: scopes, ClientID: clientID, ClientSecretName: clientSecret}
			}
			m, err := c.MCPServers.Create(cmd.Context(), p)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), m)
			}
			for _, w := range m.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", w)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered MCP server %s (%s). Reference it with managedId: %s\n", m.Name, mcpMode(*m), m.Name)
			if m.OAuth != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Connect it: openfactory mcp-registry connect %s\n", m.Name)
			}
			return nil
		},
	}
	f := add.Flags()
	f.StringVar(&desc, "description", "", "description shown in the dashboard")
	f.StringVar(&url, "url", "", "streamable HTTP endpoint (or legacy SSE when the path ends in /sse)")
	f.StringVar(&command, "command", "", "executable started inside the sandbox (stdio transport)")
	f.StringArrayVar(&margs, "arg", nil, "command argument (repeatable)")
	f.StringArrayVar(&headers, "header", nil, "HTTP header NAME=VALUE for url servers (repeatable; credentials as {{SECRET}})")
	f.StringArrayVar(&env, "env", nil, "environment variable NAME=VALUE for command servers (repeatable; credentials as {{SECRET}})")
	f.BoolVar(&direct, "direct", false, "connect from the sandbox, for a server the control plane cannot reach (headers go to the sandbox)")
	f.BoolVar(&oauth, "oauth", false, "the server uses OAuth (needs the proxy); connect it afterwards")
	f.StringArrayVar(&scopes, "scope", nil, "OAuth scope to request (repeatable; default: the server's advertised scopes)")
	f.StringVar(&clientID, "client-id", "", "pre-registered OAuth client id (default: dynamic client registration)")
	f.StringVar(&clientSecret, "client-secret-name", "", "team secret holding the OAuth client secret")
	cmd.AddCommand(add)

	cmd.AddCommand(&cobra.Command{
		Use: "delete <name>", Short: "Delete a registered MCP server and its stored tokens", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			if err := c.MCPServers.Delete(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Deleted MCP server", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "connect <name>",
		Short: "Start the OAuth connection of a server",
		Long: `Print the provider's authorization URL. Open it in a browser where you are
signed in to the dashboard as the same user as the API key: the provider
redirects back to the server, which stores the tokens.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			u, err := c.MCPServers.Connect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Open this URL in a browser signed in to the dashboard:")
			fmt.Fprintln(cmd.OutOrStdout(), u)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use: "disconnect <name>", Short: "Delete the stored OAuth tokens of a server", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			if _, err := c.MCPServers.Disconnect(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Disconnected MCP server", args[0])
			return nil
		},
	})
	return cmd
}

func mcpMode(m sdk.RegisteredMCPServer) string {
	switch {
	case m.OAuth != nil:
		return "oauth, " + strings.ReplaceAll(m.OAuth.Status, "_", " ")
	case m.Command != "":
		return "command"
	case m.Connection == "proxy":
		return "proxied"
	default:
		return "direct"
	}
}

func keyValues(flag string, items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s %q: want NAME=VALUE", flag, it)
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}
