package mcpserver

import (
	"fmt"
	"github.com/Pippit-dev/pippit-cli/internal/publicapp"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	servercore "github.com/Pippit-dev/pippit-cli/internal/mcpserver"
	"github.com/spf13/cobra"
)

// NewCommand creates the native MCP command tree.
func NewCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Expose Pippit CLI capabilities through MCP",
		Args:  cobra.NoArgs,
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.AddCommand(newServeCommand(stdout, stderr, runner))
	cmd.AddCommand(newMigrateCommand(stdout, stderr))
	return cmd
}

func newServeCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	options := servercore.DefaultOptions()
	mode := "local"
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve Pippit tools over Streamable HTTP MCP",
		Long: "Serve the Pippit/Xiaoyunque creative, short-drama, video, and Canvas capabilities as MCP tools. " +
			"The server reuses pippit-tool-cli login credentials and accepts ChatGPT file references for image-to-video workflows.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if mode == "public" {
				cfg, err := publicapp.ConfigFromEnv()
				if err != nil {
					return err
				}
				if cmd.Flags().Changed("listen") {
					cfg.Listen = options.Listen
				}
				return publicapp.Run(ctx, cfg, stdout)
			}
			if mode != "local" {
				return fmt.Errorf("mode must be local or public")
			}
			if err := servercore.Run(ctx, options, runner, stdout); err != nil {
				return fmt.Errorf("run MCP server: %w", err)
			}
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	flags := cmd.Flags()
	flags.StringVar(&mode, "mode", mode, "local uses CLI login; public requires PostgreSQL and per-user OAuth")
	flags.StringVar(&options.Listen, "listen", options.Listen, "listen address, for example 127.0.0.1:8787")
	flags.StringVar(&options.Path, "path", options.Path, "Streamable HTTP MCP endpoint path")
	flags.StringVar(&options.HealthPath, "health-path", options.HealthPath, "health-check endpoint path")
	flags.StringVar(&options.AuthToken, "auth-token", options.AuthToken, "static bearer token required from MCP clients; use PIPPIT_MCP_AUTH_TOKEN to avoid shell history")
	flags.StringSliceVar(&options.AllowedHosts, "allowed-host", options.AllowedHosts, "trusted Host header pattern; repeat or use comma-separated values")
	flags.StringSliceVar(&options.AllowedOrigins, "allowed-origin", options.AllowedOrigins, "trusted browser Origin such as https://chatgpt.com; repeat or use comma-separated values")
	flags.StringVar(&options.OutputDir, "output-dir", options.OutputDir, "server-controlled directory for downloaded results")
	flags.StringVar(&options.CLICommand, "cli-command", options.CLICommand, "npm wrapper command used for canvas command list/describe/run")
	flags.BoolVar(&options.AllowPrivateFileURLs, "allow-private-file-urls", options.AllowPrivateFileURLs, "allow HTTP and private-network file URLs; intended only for local testing")
	flags.Int64Var(&options.MaxFileBytes, "max-file-bytes", options.MaxFileBytes, "maximum bytes downloaded for one ChatGPT/reference file, capped at 200 MiB")
	flags.Int64Var(&options.MaxRequestBodyBytes, "max-request-body-bytes", options.MaxRequestBodyBytes, "maximum MCP JSON request body size")
	return cmd
}
