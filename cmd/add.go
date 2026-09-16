package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

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

func validateName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("name %q contains invalid character %q (use letters, digits, - or _)", name, r)
		}
	}
	return nil
}

func resolveSecret(cmd *cobra.Command, flag string) (string, error) {
	switch flag {
	case "":
		return promptSecret(cmd.OutOrStdout(), "Secret (input hidden): ")
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
