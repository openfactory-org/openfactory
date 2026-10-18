package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/pkg/sdk"
)

// ---------- bootstrap ----------

func newBootstrapCmd(g *globals) *cobra.Command {
	var team, email, name string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the first team, admin user and API key on a fresh server",
		Long: `Bootstrap a server that has no teams yet. Prints the admin API key once;
store it safely (export OF_API_KEY=...). Fails when the server is already
bootstrapped.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if email == "" {
				return errors.New("--email is required")
			}
			c := sdk.NewClient(strings.TrimRight(g.server, "/"), "", sdk.WithUserAgent("openfactory/"+version))
			res, err := c.Platform.Bootstrap(cmd.Context(), team, email, name)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Team:    %s (%s)\nAdmin:   %s\nAPI key: %s\n\nThe key is shown only once:\n  export OF_SERVER=%s OF_API_KEY=%s\n",
				res.Team.Name, res.Team.ID, res.User.Email, res.Key, g.server, res.Key)
			return nil
		},
	}
	cmd.Flags().StringVar(&team, "team", "Default", "team name")
	cmd.Flags().StringVar(&email, "email", "", "admin e-mail (required)")
	cmd.Flags().StringVar(&name, "name", "", "admin display name")
	return cmd
}

// ---------- factory ----------

// resolveFactory accepts a factory UID, alias or name.
func resolveFactory(ctx context.Context, c *sdk.Client, ref string) (*sdk.Factory, error) {
	if f, err := c.Factories.Get(ctx, ref); err == nil {
		return f, nil
	} else if !sdk.IsNotFound(err) && !sdk.IsInvalid(err) {
		return nil, err
	}
	all, err := c.Factories.List(ctx, nil).Collect(ctx)
	if err != nil {
		return nil, err
	}
	want := strings.TrimPrefix(ref, "@")
	for i := range all {
		if strings.EqualFold(all[i].Alias, want) || strings.EqualFold(all[i].Name, ref) {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("factory %q not found", ref)
}

func newFactoryCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "factory", Aliases: []string{"factories"}, Short: "Manage factories"}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List factories", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			fs, err := c.Factories.List(cmd.Context(), nil).Collect(cmd.Context())
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), fs)
			}
			rows := make([][]string, 0, len(fs))
			for _, f := range fs {
				rows = append(rows, []string{f.UID, f.Name, "@" + f.Alias, f.Source, ago(f.UpdatedAt)})
			}
			table(cmd.OutOrStdout(), []string{"UID", "NAME", "ALIAS", "SOURCE", "UPDATED"}, rows)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use: "get <factory>", Short: "Show a factory (UID, alias or name)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), f)
			}
			agents, _ := c.Factories.Agents(cmd.Context(), f.UID)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "UID:      %s\nName:     %s\nAlias:    @%s\nSource:   %s\n", f.UID, f.Name, f.Alias, f.Source)
			if f.Description != "" {
				fmt.Fprintf(out, "About:    %s\n", f.Description)
			}
			if len(agents) > 0 {
				fmt.Fprintln(out, "\nAgents:")
				rows := make([][]string, 0, len(agents))
				for _, a := range agents {
					rows = append(rows, []string{a.Name, a.Type, a.Harness.Type, a.Harness.Model})
				}
				table(out, []string{"NAME", "TYPE", "HARNESS", "MODEL"}, rows)
			}
			return nil
		},
	})

	var (
		cName, cAlias, cDesc, cModel, cFrom, cSource, cGitRepo, cGitBranch, cGitPath string
		cRepos, cAgents                                                              []string
	)
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a factory (default agents, or the definition in --from)",
		Long: `Create a factory. Without --from, the server generates the default
definition (foreman plus the default stage agents) for --repo repositories.
With --from DIR, the definition tree in DIR is uploaded. --source git binds the
factory to a definition repository instead (--git-repo, --git-branch, --git-path).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			p := sdk.CreateFactoryParams{Name: cName, Alias: cAlias, Description: cDesc, Model: cModel,
				Source: cSource, GitRepo: cGitRepo, GitBranch: cGitBranch, GitPath: cGitPath}
			for _, r := range cRepos {
				ref, err := engine.ParseRepoSpec(r)
				if err != nil {
					return err
				}
				p.Repositories = append(p.Repositories, sdk.RepoRef{CodeForge: ref.CodeForge, Owner: ref.Owner, Name: ref.Name})
			}
			if cmd.Flags().Changed("agents") {
				a := cAgents
				p.Agents = &a
			}
			if cFrom != "" {
				files, err := definition.LoadDir(cFrom)
				if err != nil {
					return err
				}
				p.Files = files
			}
			res, err := c.Factories.Create(cmd.Context(), p)
			if err != nil {
				return explain(err)
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created factory %s (@%s): %s\n", res.Factory.Name, res.Factory.Alias, res.Factory.UID)
			return nil
		},
	}
	f := create.Flags()
	f.StringVar(&cName, "name", "", "factory name")
	f.StringVar(&cAlias, "alias", "", "foreman @alias")
	f.StringVar(&cDesc, "description", "", "description")
	f.StringVar(&cModel, "model", "", "default model for generated agents")
	f.StringArrayVar(&cRepos, "repo", nil, "repository [forge:]owner/name (repeatable)")
	f.StringSliceVar(&cAgents, "agents", nil, "default stage agents to include")
	f.StringVar(&cFrom, "from", "", "upload the definition tree in this directory")
	f.StringVar(&cSource, "source", "", "definition source: managed (default) or git")
	f.StringVar(&cGitRepo, "git-repo", "", "definition repository for --source git (e.g. github.com/acme/factory)")
	f.StringVar(&cGitBranch, "git-branch", "", "production branch for --source git (default main)")
	f.StringVar(&cGitPath, "git-path", "", "directory holding factory.yaml in the definition repository")
	cmd.AddCommand(create)

	var applyFactory, applyMessage string
	apply := &cobra.Command{
		Use:   "apply <dir>",
		Short: "Validate and apply a definition tree to a managed factory",
		Long: `Upload the definition tree in dir as a new version of a managed factory.
The target is --factory, otherwise the factory whose alias matches the
definition's alias (or name); a new factory is created when none exists.
Invalid definitions are rejected and the last valid version stays active.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			files, err := definition.LoadDir(args[0])
			if err != nil {
				return err
			}
			def, verrs := definition.Compile(files)
			if len(verrs) > 0 {
				for _, e := range verrs {
					fmt.Fprintln(cmd.ErrOrStderr(), formatValidationError(args[0], e))
				}
				return errSilent
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			ref := applyFactory
			if ref == "" {
				ref = def.Alias
				if ref == "" {
					ref = def.Name
				}
			}
			f, err := resolveFactory(ctx, c, ref)
			if err != nil {
				if applyFactory != "" {
					return err
				}
				res, err := c.Factories.Create(ctx, sdk.CreateFactoryParams{Files: files})
				if err != nil {
					return explain(err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Created factory %s (@%s): %s\n", res.Factory.Name, res.Factory.Alias, res.Factory.UID)
				return nil
			}
			v, err := c.Definitions.Put(ctx, f.UID, sdk.PutDefinitionParams{Files: files, Message: applyMessage})
			if err != nil {
				return explain(err)
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), v)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Applied version %s to %s (@%s)\n", v.ID, f.Name, f.Alias)
			return nil
		},
	}
	apply.Flags().StringVar(&applyFactory, "factory", "", "target factory (UID, alias or name)")
	apply.Flags().StringVarP(&applyMessage, "message", "m", "Applied with openfactory CLI", "version message")
	cmd.AddCommand(apply)

	cmd.AddCommand(newDispatchCmd(g, "run <factory> <prompt>", "Send a work item to a factory's foreman"))
	return cmd
}

// newDispatchCmd sends a prompt to a factory (factory run / work-item send).
func newDispatchCmd(g *globals, use, short string) *cobra.Command {
	var title, agent string
	var follow bool
	var meta []string
	cmd := &cobra.Command{
		Use: use, Short: short, Args: cobra.MinimumNArgs(2),
		Long: short + `. The prompt may be several words; "-" reads it from stdin.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			prompt := strings.Join(args[1:], " ")
			if prompt == "-" {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				prompt = string(b)
			}
			md, err := parseKV(meta, "--metadata")
			if err != nil {
				return err
			}
			r, err := c.Factories.Dispatch(ctx, f.UID, sdk.DispatchParams{Prompt: prompt, Title: title, Agent: agent, Metadata: md})
			if err != nil {
				return explain(err)
			}
			if g.json() && !follow {
				return emit(cmd.OutOrStdout(), r)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Started run %s (work item %s)\n%s\n", r.RunID, r.WorkItemID, r.RunURL)
			if follow {
				return streamLogs(ctx, cmd.OutOrStdout(), c, r.RunID, true)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "work item title")
	cmd.Flags().StringVar(&agent, "agent", "", "agent to start (default: the foreman)")
	cmd.Flags().StringArrayVar(&meta, "metadata", nil, "run metadata key=value (repeatable)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream the run transcript")
	return cmd
}

func parseKV(list []string, flag string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid %s %q (want key=value)", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

// explain expands validation errors returned by the API.
func explain(err error) error {
	var e *sdk.Error
	if errors.As(err, &e) && len(e.Errors) > 0 {
		var b strings.Builder
		b.WriteString(e.Error())
		for _, v := range e.Errors {
			loc := v.File
			if v.Line > 0 {
				loc += ":" + strconv.Itoa(v.Line)
			}
			fmt.Fprintf(&b, "\n  %s: %s", loc, v.Message)
		}
		return errors.New(b.String())
	}
	return err
}

// ---------- runs ----------

func newRunCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "run", Aliases: []string{"runs"}, Short: "Inspect and control agent runs"}

	var factory, state, workItem string
	var limit int
	list := &cobra.Command{
		Use: "list", Short: "List recent runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			opts := &sdk.RunListOptions{WorkItemID: workItem, PageSize: limit}
			if factory != "" {
				f, err := resolveFactory(ctx, c, factory)
				if err != nil {
					return err
				}
				opts.FactoryID = f.UID
			}
			if state != "" {
				opts.States = strings.Split(strings.ToUpper(state), ",")
			}
			it := c.Runs.List(ctx, opts)
			var runs []sdk.Run
			for it.Next(ctx) && len(runs) < limit {
				runs = append(runs, it.Value())
			}
			if err := it.Err(); err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), runs)
			}
			rows := make([][]string, 0, len(runs))
			for _, r := range runs {
				rows = append(rows, []string{r.RunID, r.State, r.Name, r.AgentName, ago(r.CreatedAt), trunc(r.Title, 50)})
			}
			table(cmd.OutOrStdout(), []string{"RUN", "STATE", "NAME", "AGENT", "CREATED", "TITLE"}, rows)
			return nil
		},
	}
	list.Flags().StringVar(&factory, "factory", "", "only runs of this factory")
	list.Flags().StringVar(&state, "state", "", "comma-separated states (queued,pending,claimed,running,blocked,succeeded,failed,error,cancelled)")
	list.Flags().StringVar(&workItem, "work-item", "", "only runs of this work item")
	list.Flags().IntVar(&limit, "limit", 20, "maximum runs to show")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use: "get <run>", Short: "Show a run", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			r, err := c.Runs.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), r)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Run:      %s\nState:    %s\nAgent:    %s (%s)\nCreated:  %s\nURL:      %s\n", r.RunID, r.State, r.AgentName, r.Name, r.CreatedAt.Local().Format(time.RFC3339), r.RunURL)
			if r.ParentRunID != "" {
				fmt.Fprintf(out, "Parent:   %s\n", r.ParentRunID)
			}
			fmt.Fprintf(out, "Usage:    %d in / %d out tokens, $%.4f\n", r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CostUSD)
			if r.Error != "" {
				fmt.Fprintf(out, "Error:    %s\n", r.Error)
			}
			if r.Result != "" {
				fmt.Fprintf(out, "\n%s\n", r.Result)
			}
			return nil
		},
	})

	var follow bool
	logs := &cobra.Command{
		Use: "logs <run>", Short: "Print a run's transcript", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			return streamLogs(cmd.Context(), cmd.OutOrStdout(), c, args[0], follow)
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the run finishes its turn")
	cmd.AddCommand(logs)

	var fuFollow bool
	followup := &cobra.Command{
		Use: "followup <run> <message>", Short: "Send a follow-up message to a run", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			r, err := c.Runs.FollowUp(cmd.Context(), args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Follow-up queued for %s (%s)\n", r.RunID, r.State)
			if fuFollow {
				return streamLogs(cmd.Context(), cmd.OutOrStdout(), c, r.RunID, true)
			}
			return nil
		},
	}
	followup.Flags().BoolVarP(&fuFollow, "follow", "f", false, "stream the transcript afterwards")
	cmd.AddCommand(followup)

	cmd.AddCommand(&cobra.Command{
		Use: "cancel <run>", Short: "Cancel a run and its child runs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			if err := c.Runs.Cancel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Cancelled", args[0])
			return nil
		},
	})
	return cmd
}

// streamLogs prints run messages; with follow it polls until the run is idle or finished.
func streamLogs(ctx context.Context, w io.Writer, c *sdk.Client, runID string, follow bool) error {
	var after int64
	for {
		msgs, err := c.Runs.Messages(ctx, runID, after)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			printMessage(w, m)
			if m.Seq > after {
				after = m.Seq
			}
		}
		if !follow {
			return nil
		}
		r, err := c.Runs.Get(ctx, runID)
		if err != nil {
			return err
		}
		if r.Finished() || r.WaitsForInput() {
			// Drain anything written after the last poll.
			if msgs, err := c.Runs.Messages(ctx, runID, after); err == nil {
				for _, m := range msgs {
					printMessage(w, m)
				}
			}
			fmt.Fprintf(w, "-- run %s is %s\n", runID, r.State)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func printMessage(w io.Writer, m sdk.RunMessage) {
	ts := m.CreatedAt.Local().Format("15:04:05")
	content := strings.TrimRight(m.Content, "\n")
	switch m.Role {
	case "tool_call", "tool_result", "log":
		content = trunc(content, 200)
	}
	fmt.Fprintf(w, "%s %-11s %s\n", ts, m.Role, content)
}

// ---------- work items ----------

func newWorkItemCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "work-item", Short: "Work items: list, inspect, send and complete work items"}

	var stage string
	list := &cobra.Command{
		Use: "list <factory>", Short: "List a factory's work items", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			opts := &sdk.WorkItemListOptions{}
			if stage != "" {
				opts.Stages = strings.Split(stage, ",")
			}
			items, err := c.WorkItems.List(ctx, f.UID, opts).Collect(ctx)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), items)
			}
			rows := make([][]string, 0, len(items))
			for _, w := range items {
				rows = append(rows, []string{w.ID, w.Stage, w.Source.Provider, ago(w.UpdatedAt), trunc(w.Title, 60)})
			}
			table(cmd.OutOrStdout(), []string{"ID", "STAGE", "SOURCE", "UPDATED", "TITLE"}, rows)
			return nil
		},
	}
	list.Flags().StringVar(&stage, "stage", "", "comma-separated stages")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use: "get <id>", Short: "Show a work item with its timeline", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			d, err := c.WorkItems.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), d)
		},
	})
	cmd.AddCommand(newDispatchCmd(g, "send <factory> <prompt>", "Send a new work item to a factory"))

	var note string
	complete := &cobra.Command{
		Use: "complete <id>", Short: "Mark a work item complete", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			w, err := c.WorkItems.Complete(cmd.Context(), args[0], note)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Work item %s is %s\n", w.ID, w.Stage)
			return nil
		},
	}
	complete.Flags().StringVar(&note, "note", "", "closing note")
	cmd.AddCommand(complete)
	return cmd
}

// ---------- inbox ----------

func newInboxCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "inbox", Short: "Approvals, questions and handoffs waiting on people"}
	var factory string
	var all, team bool
	list := &cobra.Command{
		Use: "list", Short: "List the open inbox items for you", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			opts := &sdk.InboxListOptions{IncludeResolved: all, Team: team}
			if factory != "" {
				f, err := resolveFactory(ctx, c, factory)
				if err != nil {
					return err
				}
				opts.FactoryID = f.UID
			}
			items, err := c.Inbox.List(ctx, opts).Collect(ctx)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), items)
			}
			rows := make([][]string, 0, len(items))
			for _, it := range items {
				st := "open"
				if it.Resolved {
					st = it.Resolution
				}
				rows = append(rows, []string{it.ID, it.Kind, st, ago(it.CreatedAt), trunc(it.Title, 60)})
			}
			table(cmd.OutOrStdout(), []string{"ID", "KIND", "STATUS", "CREATED", "TITLE"}, rows)
			return nil
		},
	}
	list.Flags().StringVar(&factory, "factory", "", "only items of this factory")
	list.Flags().BoolVar(&all, "all", false, "include resolved items")
	list.Flags().BoolVar(&team, "team", false, "list the items of all people (editor or admin)")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use:   "unresolve <id>",
		Short: "Undo a resolve (within 10 minutes, before the run reads the answer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			it, err := c.Inbox.Unresolve(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), it)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reopened %s\n", it.ID)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "resolve <id> <resolution>",
		Short: "Resolve an inbox item (approve, reject, or an answer)",
		Long:  "Resolve an inbox item. Approvals take approve or reject; questions take the answer text.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			it, err := c.Inbox.Resolve(cmd.Context(), args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Resolved %s: %s\n", it.ID, it.Resolution)
			return nil
		},
	})
	return cmd
}

// ---------- secrets ----------

func newSecretCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Aliases: []string{"secrets"}, Short: "Manage encrypted secrets"}
	cmd.AddCommand(newSecretRewrapCmd())
	var factory, typ string
	scope := func(ctx context.Context, c *sdk.Client) (string, error) {
		if factory == "" {
			return "", nil
		}
		f, err := resolveFactory(ctx, c, factory)
		if err != nil {
			return "", err
		}
		return f.UID, nil
	}
	set := &cobra.Command{
		Use:   "set <name> [value]",
		Short: "Create or update a secret (value from the argument, or stdin when omitted or -)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			fid, err := scope(ctx, c)
			if err != nil {
				return err
			}
			var value string
			if len(args) == 2 && args[1] != "-" {
				value = args[1]
			} else {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				value = strings.TrimRight(string(b), "\r\n")
			}
			s, err := c.Secrets.Create(ctx, args[0], typ, value, fid)
			if sdk.IsConflict(err) {
				s, err = c.Secrets.Update(ctx, args[0], value, fid)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved secret %s (%s)\n", s.Name, s.Type)
			return nil
		},
	}
	set.Flags().StringVar(&typ, "type", "generic", "secret type: generic, anthropic_api_key, openai_api_key, docker_registry, ...")
	cmd.AddCommand(set)
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List secrets (names only)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			fid, err := scope(ctx, c)
			if err != nil {
				return err
			}
			list, err := c.Secrets.List(ctx, fid)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), list)
			}
			rows := make([][]string, 0, len(list))
			for _, s := range list {
				sc := "team"
				if s.FactoryID != "" {
					sc = s.FactoryID
				}
				rows = append(rows, []string{s.Name, s.Type, sc, ago(s.UpdatedAt)})
			}
			table(cmd.OutOrStdout(), []string{"NAME", "TYPE", "SCOPE", "UPDATED"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "delete <name>", Short: "Delete a secret", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			fid, err := scope(ctx, c)
			if err != nil {
				return err
			}
			if err := c.Secrets.Delete(ctx, args[0], fid); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Deleted secret", args[0])
			return nil
		},
	})
	cmd.PersistentFlags().StringVar(&factory, "factory", "", "factory scope (default: team scope)")
	return cmd
}

// ---------- webhooks ----------

func newWebhookCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "webhook", Aliases: []string{"webhooks"}, Short: "Manage custom webhooks"}
	cmd.AddCommand(&cobra.Command{
		Use: "list <factory>", Short: "List a factory's webhooks", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			list, err := c.Webhooks.List(ctx, f.UID)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), list)
			}
			rows := make([][]string, 0, len(list))
			for _, w := range list {
				rows = append(rows, []string{w.ID, w.Name, w.AuthMode, w.SignatureScheme, strconv.FormatBool(w.Enabled)})
			}
			table(cmd.OutOrStdout(), []string{"ID", "NAME", "AUTH", "SCHEME", "ENABLED"}, rows)
			return nil
		},
	})
	var p sdk.CreateWebhookParams
	create := &cobra.Command{
		Use: "create <factory>", Short: "Create a webhook on a managed factory", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			res, err := c.Webhooks.Create(ctx, f.UID, p)
			if err != nil {
				return explain(err)
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created webhook %s (%s)\nIngress URL: %s\n", res.Webhook.Name, res.Webhook.ID, res.IngressURL)
			if res.Secret != "" {
				fmt.Fprintf(out, "Secret (shown once): %s\n", res.Secret)
			}
			if res.AwaitingSecret {
				fmt.Fprintln(out, "Set the provider's signing secret with the dashboard or `webhook` API rotate-secret.")
			}
			return nil
		},
	}
	cf := create.Flags()
	cf.StringVar(&p.Name, "name", "", "webhook name (required)")
	cf.StringVar(&p.AuthMode, "auth-mode", "token", "token, url_token or signature")
	cf.StringVar(&p.SignatureScheme, "signature-scheme", "", "github, pagerduty, sentry, standard_webhooks, stripe or vercel (auth-mode signature)")
	cf.StringVar(&p.SecretName, "secret-name", "", "secret holding the credential (default derived from the name)")
	cf.StringVar(&p.DeliveryIDHeader, "delivery-id-header", "", "header carrying a unique delivery id (deduplication)")
	cmd.AddCommand(create)

	var limit int
	deliveries := &cobra.Command{
		Use: "deliveries <webhook-id>", Short: "Show recent deliveries of a webhook", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			list, err := c.Webhooks.Deliveries(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), list)
			}
			rows := make([][]string, 0, len(list))
			for _, d := range list {
				rows = append(rows, []string{d.ID, d.Status, d.Reason, strings.Join(d.RunIDs, ","), ago(d.ReceivedAt)})
			}
			table(cmd.OutOrStdout(), []string{"ID", "STATUS", "REASON", "RUNS", "RECEIVED"}, rows)
			return nil
		},
	}
	deliveries.Flags().IntVar(&limit, "limit", 20, "maximum deliveries")
	cmd.AddCommand(deliveries)
	return cmd
}

// ---------- benchmarks ----------

func newBenchmarkCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "benchmark", Aliases: []string{"benchmarks"}, Short: "Run and inspect benchmark suites"}
	var configs, scorers []string
	var reps int
	runCmd := &cobra.Command{
		Use: "run <factory> <suite>", Short: "Launch a benchmark suite", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRepetitions(reps); err != nil {
				return err
			}
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			p := sdk.StartBenchmarkParams{Scorers: scorers, Repetitions: reps}
			for _, n := range configs {
				p.Configurations = append(p.Configurations, sdk.BenchmarkConfiguration{Name: n})
			}
			br, err := c.Measure.StartBenchmark(ctx, f.UID, args[1], p)
			if err != nil {
				return explain(err)
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), br)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Started benchmark run %s (%s)\n", br.ID, br.State)
			return nil
		},
	}
	runCmd.Flags().StringSliceVar(&configs, "config", nil, "configuration names to compare (default: the suite's)")
	runCmd.Flags().StringSliceVar(&scorers, "scorer", nil, "additional scorers (ID or slug)")
	runCmd.Flags().IntVar(&reps, "repetitions", 1, "repetitions per configuration and task (1 to 10)")
	cmd.AddCommand(runCmd)

	var suite string
	list := &cobra.Command{
		Use: "list <factory>", Short: "List benchmark suites and recent runs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			f, err := resolveFactory(ctx, c, args[0])
			if err != nil {
				return err
			}
			suites, err := c.Measure.Benchmarks(ctx, f.UID)
			if err != nil {
				return err
			}
			runs, err := c.Measure.BenchmarkRuns(ctx, f.UID, suite).Collect(ctx)
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), map[string]any{"suites": suites, "runs": runs})
			}
			out := cmd.OutOrStdout()
			rows := make([][]string, 0, len(suites))
			for _, s := range suites {
				rows = append(rows, []string{s.Slug, s.Name, s.Agent, strconv.Itoa(len(s.Configurations)), strconv.Itoa(len(s.Tasks))})
			}
			table(out, []string{"SUITE", "NAME", "AGENT", "CONFIGS", "TASKS"}, rows)
			if len(runs) > 0 {
				fmt.Fprintln(out)
				rows = rows[:0]
				for _, r := range runs {
					rows = append(rows, []string{r.ID, r.Suite, r.State, ago(r.CreatedAt)})
				}
				table(out, []string{"RUN", "SUITE", "STATE", "CREATED"}, rows)
			}
			return nil
		},
	}
	list.Flags().StringVar(&suite, "suite", "", "only runs of this suite")
	cmd.AddCommand(list)
	return cmd
}

// ---------- workers ----------

func newWorkerListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List registered remote workers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			ws, err := c.Platform.Workers(cmd.Context())
			if err != nil {
				return err
			}
			if g.json() {
				return emit(cmd.OutOrStdout(), ws)
			}
			rows := make([][]string, 0, len(ws))
			for _, w := range ws {
				capStr := "unlimited"
				if w.Capacity > 0 {
					capStr = strconv.Itoa(w.Capacity)
				}
				rows = append(rows, []string{w.ID, w.Status, w.Backend, fmt.Sprintf("%d/%s", w.Active, capStr), w.Version, ago(w.LastHeartbeat)})
			}
			table(cmd.OutOrStdout(), []string{"ID", "STATUS", "BACKEND", "ACTIVE", "VERSION", "HEARTBEAT"}, rows)
			return nil
		},
	}
}

// maxBenchmarkRepetitions matches the API and dashboard limit.
const maxBenchmarkRepetitions = 10

// validateRepetitions rejects repetitions outside 1..10 before calling the API.
func validateRepetitions(n int) error {
	if n < 1 || n > maxBenchmarkRepetitions {
		return fmt.Errorf("--repetitions must be between 1 and %d (got %d)", maxBenchmarkRepetitions, n)
	}
	return nil
}
