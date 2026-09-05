package mcpserver

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	server "github.com/Pippit-dev/pippit-cli/internal/mcpserver"
	"github.com/spf13/cobra"
)

// NewCommand creates the `pippit-tool-cli mcp` command group.
func NewCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	command := &cobra.Command{
		Use:   "mcp",
		Short: "Serve Pippit capabilities through Model Context Protocol",
		Long:  "Expose Pippit / Xiaoyunque creative operations as a Streamable HTTP MCP server for ChatGPT and other MCP clients.",
	}
	command.AddCommand(newServeCommand(stdout, stderr, runner))
	return command
}

func newServeCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	opts := server.ServeOptions{}
	command := &cobra.Command{
		Use:   "serve",
		Short: "Start the Streamable HTTP MCP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.BearerToken == "" {
				opts.BearerToken = os.Getenv(server.BearerTokenEnv)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := server.Serve(ctx, opts, runner, stdout, stderr); err != nil {
				return fmt.Errorf("启动 MCP 服务失败: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&opts.ListenAddress, "listen", server.DefaultListenAddress, "HTTP listen address")
	command.Flags().StringVar(&opts.MCPPath, "path", server.DefaultMCPPath, "MCP endpoint path")
	command.Flags().StringVar(&opts.Workspace, "workspace", server.DefaultWorkspace(), "server-side workspace for downloaded results")
	command.Flags().StringVar(&opts.AllowedOrigin, "allow-origin", "*", "Access-Control-Allow-Origin value")
	command.Flags().BoolVar(&opts.AllowInsecurePublic, "allow-insecure-public", false, "allow a non-loopback listener without bearer authentication")
	command.Flags().StringVar(&opts.BearerToken, "bearer-token", "", "optional MCP endpoint bearer token (prefer PIPPIT_MCP_BEARER_TOKEN)")
	return command
}
