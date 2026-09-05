package chatgpt_app

import (
	"io"
	"os"

	"github.com/Pippit-dev/pippit-cli/internal/chatgpt_app"
	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/spf13/cobra"
)

const envMCPAuthToken = "PIPPIT_CHATGPT_MCP_TOKEN"

// NewCommand builds the ChatGPT App command group.
func NewCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	command := &cobra.Command{
		Use:   "chatgpt-app",
		Short: "Run the private ChatGPT MCP bridge for Xiaoyunque",
		Args:  cobra.NoArgs,
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.AddCommand(newServeCommand(stdout, stderr, runner))
	return command
}

func newServeCommand(stdout, stderr io.Writer, runner *common.Runner) *cobra.Command {
	opts := chatgptapp.ServeOptions{}
	command := &cobra.Command{
		Use:   "serve",
		Short: "Serve Xiaoyunque tools over Streamable HTTP MCP",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return chatgptapp.Serve(command.Context(), opts, runner, stdout, stderr)
		},
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	flags := command.Flags()
	flags.StringVar(&opts.ListenAddress, "listen", chatgptapp.DefaultListenAddress, "listen address for the local HTTP server")
	flags.StringVar(&opts.MCPPath, "path", chatgptapp.DefaultMCPPath, "HTTP path for the MCP endpoint")
	flags.StringVar(&opts.AuthToken, "auth-token", os.Getenv(envMCPAuthToken), "optional bearer token required by the MCP endpoint (or PIPPIT_CHATGPT_MCP_TOKEN)")
	return command
}
