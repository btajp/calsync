package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/btajp/calsync/internal/auth"
	"github.com/btajp/calsync/internal/config"
)

var (
	authDeviceCode bool
	authPort       int
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage OAuth tokens",
}

var authAddCmd = &cobra.Command{
	Use:   "add <account-id>",
	Short: "Authorize an account and store its token (run on the host, not inside the container)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		acct := cfg.AccountByID(args[0])
		if acct == nil {
			return fmt.Errorf("account %q is not defined in %s", args[0], flagConfig)
		}
		ocfg, err := oauthConfigFor(cfg, *acct)
		if err != nil {
			return err
		}
		var tok *oauth2.Token
		if authDeviceCode {
			if acct.Provider != "microsoft" {
				return errors.New("--device-code is available only for microsoft accounts")
			}
			tok, err = auth.RunDeviceFlow(cmd.Context(), ocfg)
		} else {
			tok, err = auth.RunLoopbackFlow(cmd.Context(), ocfg, authPort)
		}
		if err != nil {
			return err
		}
		tokens := &auth.TokenStore{Dir: flagData}
		if err := tokens.Save(acct.ID, tok); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "token saved for account %s\n", acct.ID)
		return nil
	},
}

var authListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show token status for each configured account",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		tokens := &auth.TokenStore{Dir: flagData}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ACCOUNT\tPROVIDER\tTOKEN")
		for _, acct := range cfg.Accounts {
			state := "ok"
			tok, err := tokens.Load(acct.ID)
			switch {
			case err != nil:
				state = fmt.Sprintf("missing (run: calsync auth add %s)", acct.ID)
			case tok.RefreshToken == "":
				state = fmt.Sprintf("no refresh token (re-run: calsync auth add %s)", acct.ID)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", acct.ID, acct.Provider, state)
		}
		return w.Flush()
	},
}

func init() {
	authAddCmd.Flags().BoolVar(&authDeviceCode, "device-code", false, "use the OAuth device code flow (microsoft only)")
	authAddCmd.Flags().IntVar(&authPort, "port", 0, "fixed loopback port for the OAuth redirect (0 = random; set when publishing a Docker port)")
	authCmd.AddCommand(authAddCmd, authListCmd)
	rootCmd.AddCommand(authCmd)
}
