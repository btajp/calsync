package main

import (
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/appserver"
)

var appserverCmd = &cobra.Command{
	Use:   "appserver",
	Short: "Run the localhost API server for the calsync desktop app (spawned as a Tauri sidecar)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := appserver.GenerateToken()
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		// 親(Tauri 殻)が死んだら stdin が EOF になり自動終了する(孤児化防止)
		appserver.WatchStdinEOF(cmd.InOrStdin(), cancel)
		s := appserver.New(flagConfig, flagData, token)
		return s.Serve(ctx, ln, cmd.OutOrStdout())
	},
}

func init() { rootCmd.AddCommand(appserverCmd) }
