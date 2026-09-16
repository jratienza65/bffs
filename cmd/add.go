package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/accounts"
	"github.com/jratienza65/bffs/internal/store"
)

var (
	addSecret string
	addEmail  string
	addForce  bool
)

var addCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a new api_key account (use `bffs login <name>` for oauth)",
	Long: `Adds an api_key account by recording its sk-ant-... key.

For oauth accounts, use ` + "`bffs login <name>`" + ` instead — oauth is per-account
session-dir backed and requires a browser flow that bffs cannot replicate
from a manually-pasted blob.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		if _, exists := accs.Accounts[name]; exists && !addForce {
			return fmt.Errorf("account %q already exists (use --force to overwrite)", name)
		}

		secret, err := resolveSecret(cmd, addSecret)
		if err != nil {
			return err
		}
		if secret == "" {
			return errors.New("secret is required")
		}

		accs.Accounts[name] = store.Account{Type: store.TypeAPIKey, Secret: secret, Email: addEmail}
		if err := store.SaveAccounts(dir, accs); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "added account %q (type=api_key)\n", name)
		return nil
	},
}

func init() {
	addCmd.Flags().StringVar(&addSecret, "secret", "", "sk-ant-... API key (prompted with no-echo if omitted; use - to read from stdin)")
	addCmd.Flags().StringVar(&addEmail, "email", "", "optional email/identifier shown in `list`")
	addCmd.Flags().BoolVar(&addForce, "force", false, "overwrite an existing account with the same name")
	rootCmd.AddCommand(addCmd)
}

// The name rules live with the engine that creates accounts
// (internal/accounts), so `add`, `login`, `rename` and the browser
// cannot drift apart.
const reservedAccountName = accounts.HomeName

func validateName(name string) error { return accounts.ValidateName(name) }

func resolveSecret(cmd *cobra.Command, flag string) (string, error) {
	switch flag {
	case "":
		return promptSecret(cmd.ErrOrStderr(), "Secret (input hidden): ")
	case "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return flag, nil
	}
}
