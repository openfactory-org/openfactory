package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/rewrap"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/store"
)

func newSecretRewrapCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "rewrap",
		Short: "Wrap the data keys of all secrets again with the current secret key",
		Long: `Read each stored secret, OAuth token of an MCP server and personal token of
a user. Unwrap its data key with any known secret key and wrap it again with the
current key (OF_SECRET_KEY). Known keys are OF_SECRET_KEY and OF_SECRET_KEY_PREVIOUS.

The command reads the same settings as "serve", and it needs the database
(OF_DATABASE_URL). Run it after you move the old key to OF_SECRET_KEY_PREVIOUS
on all replicas. Remove OF_SECRET_KEY_PREVIOUS only when the command reports no
failures. It exits with code 1 when a value failed.

With --dry-run it prints the counts and writes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags())
			if err != nil {
				return err
			}
			cur, prev, err := cfg.EncryptionKeys()
			if err != nil {
				return fmt.Errorf("secret key: %w", err)
			}
			if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
				return fmt.Errorf("create data dir: %w", err)
			}
			st, err := store.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			if c, ok := st.(io.Closer); ok {
				defer c.Close()
			}
			rep, err := rewrap.Run(cmd.Context(), st, secrets.NewWithKeys(st, cur, prev...), dryRun)
			if err != nil {
				return err
			}
			return printRewrap(cmd.OutOrStdout(), cmd.ErrOrStderr(), rep, dryRun)
		},
	}
	config.BindFlags(cmd.Flags())
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the counts and write nothing")
	return cmd
}

// printRewrap prints the counts and returns errSilent when a value failed.
func printRewrap(out, errw io.Writer, rep rewrap.Report, dryRun bool) error {
	label := "rewrapped"
	if dryRun {
		label = "would rewrap"
	}
	fmt.Fprintf(out, "%s: %d\nalready current: %d\nchanged during the run: %d\nfailed: %d\n", label, rep.Rewrapped, rep.Current, rep.Changed, rep.Failed)
	for _, e := range rep.Errors {
		fmt.Fprintln(errw, "failed:", e)
	}
	if rep.Failed > 0 {
		fmt.Fprintln(errw, "error: some values failed. Do not remove the previous key.")
		return errSilent
	}
	return nil
}
