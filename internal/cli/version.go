package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// init registers the version command.
func init() { register(newVersionCmd) }

// newVersionCmd builds the "version" command, which prints the build version.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the inferenced version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "inferenced", Version); err != nil {
				return fmt.Errorf("write version: %w", err)
			}
			return nil
		},
	}
}
