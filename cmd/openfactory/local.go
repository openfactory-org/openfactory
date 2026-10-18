package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/engine"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [dir]",
		Short: "Validate a factory definition tree (default: current directory)",
		Long: `Load the definition tree rooted at dir (the directory holding factory.yaml),
validate it against the v1 schemas and semantic rules, and compile it.
Errors are printed as file:line: message; the exit status is 1 when the
definition is invalid.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			files, err := definition.LoadDir(dir)
			if err != nil {
				return err
			}
			def, verrs := definition.Compile(files)
			out := cmd.OutOrStdout()
			if len(verrs) > 0 || def == nil {
				for _, e := range verrs {
					fmt.Fprintln(out, formatValidationError(dir, e))
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %d error(s)\n", dir, len(verrs))
				return errSilent
			}
			fmt.Fprintf(out, "%s: valid (%s) — %d agent(s), %d automation(s), %d webhook(s), %d scorer(s), %d benchmark(s)\n",
				dir, def.Name, len(def.Agents), len(def.Automations), len(def.Webhooks), len(def.Scorers), len(def.Benchmarks))
			return nil
		},
	}
}

func formatValidationError(dir string, e core.ValidationError) string {
	loc := filepath.Join(dir, filepath.FromSlash(e.File))
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
	}
	msg := e.Message
	if e.Field != "" && !strings.Contains(msg, e.Field) {
		msg = e.Field + ": " + msg
	}
	return loc + ": " + msg
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema [document]",
		Short: "List the definition JSON Schemas or print one",
		Long: `Without arguments, list the JSON Schema documents of the current definition
schema version. With a document name (e.g. factory.schema.json, or just
"factory"), print that schema.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			schemas := definition.Schemas()
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				names := make([]string, 0, len(schemas))
				for n := range schemas {
					names = append(names, n)
				}
				sort.Strings(names)
				fmt.Fprintf(out, "Schema version %s (%s)\n", definition.SchemaVersion, definition.SchemaBaseURL)
				for _, n := range names {
					fmt.Fprintln(out, "  "+n)
				}
				return nil
			}
			name := args[0]
			doc, ok := schemas[name]
			if !ok {
				doc, ok = schemas[name+".schema.json"]
			}
			if !ok {
				return fmt.Errorf("unknown schema document %q (run `openfactory schema` to list them)", name)
			}
			var v any
			if err := json.Unmarshal(doc, &v); err != nil {
				_, err = out.Write(doc)
				return err
			}
			return emit(out, v)
		},
	}
}

func newInitCmd() *cobra.Command {
	var (
		name, alias, model string
		repos, agents      []string
		force              bool
	)
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a factory definition with the default agents",
		Long: `Write a default definition tree (factory.yaml plus the foreman and the
default stage agents) into dir (default: current directory).

Example:
  openfactory init my-factory --name "Payments" --repo acme/payments --repo gitlab:acme/web`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			if name == "" {
				name = filepath.Base(abs)
			}
			var refs []core.RepoRef
			for _, r := range repos {
				ref, err := engine.ParseRepoSpec(r)
				if err != nil {
					return err
				}
				refs = append(refs, ref)
			}
			files, err := definition.DefaultDefinition(definition.DefaultDefinitionOptions{
				Name: name, Alias: alias, Repositories: refs, Model: model, Agents: agents,
			})
			if err != nil {
				return err
			}
			if !force {
				for _, p := range []string{"factory.yaml", "factory.yml"} {
					if _, err := os.Stat(filepath.Join(abs, p)); err == nil {
						return fmt.Errorf("%s already contains %s (use --force to overwrite)", dir, p)
					}
				}
			}
			paths := make([]string, 0, len(files))
			for p := range files {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				full := filepath.Join(abs, filepath.FromSlash(p))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "created", filepath.Join(dir, filepath.FromSlash(p)))
			}
			if _, verrs := definition.Compile(files); len(verrs) > 0 {
				return errors.New("scaffolded definition is invalid: " + verrs[0].Error())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nNext: edit the agents, then `openfactory validate %s` and `openfactory factory apply %s`.\n", dir, dir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "factory name (default: directory name)")
	f.StringVar(&alias, "alias", "", "foreman @alias")
	f.StringVar(&model, "model", "", "default model for all agents")
	f.StringArrayVar(&repos, "repo", nil, "repository [forge:]owner/name (repeatable; forge github, gitlab or azure_devops)")
	f.StringSliceVar(&agents, "agents", nil, "default stage agents to include (triage,spec,implement,review); default all")
	f.BoolVar(&force, "force", false, "overwrite an existing definition")
	return cmd
}
