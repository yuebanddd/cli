package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/publicapp"
	"github.com/spf13/cobra"
)

func newMigrateCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "migrate [up|down]", Short: "Apply public PostgreSQL migrations (down erases public data)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "up" && args[0] != "down" {
			return errors.New("expected up or down")
		}
		if args[0] == "down" && os.Getenv("PIPPIT_CONFIRM_DROP_PUBLIC_DATA") != "yes" {
			return errors.New("down deletes all public data; set PIPPIT_CONFIRM_DROP_PUBLIC_DATA=yes only after backup")
		}
		key, e := base64.StdEncoding.DecodeString(os.Getenv("PIPPIT_CREDENTIAL_MASTER_KEY"))
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		s, e := publicapp.Open(ctx, os.Getenv("DATABASE_URL"), key)
		if e != nil {
			return e
		}
		defer s.Close()
		if args[0] == "down" {
			return s.MigrateDown(ctx)
		}
		return s.Migrate(ctx)
	}}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd
}
