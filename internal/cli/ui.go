package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// defaultUIPort is the port the future web dashboard is intended to listen on.
const defaultUIPort = 8788

// init registers the ui command.
func init() { register(newUICmd) }

// newUICmd builds the "ui" command. It is a stub for the forthcoming web
// dashboard: it prints an availability notice and the intended port so the
// command is discoverable in help today.
func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Local web dashboard (coming soon)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, _ := cmd.Flags().GetInt("port")
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintln(out, "inferenced ui: the web dashboard arrives in the next release"); err != nil {
				return fmt.Errorf("write ui notice: %w", err)
			}
			if _, err := fmt.Fprintf(out, "intended port: %d\n", port); err != nil {
				return fmt.Errorf("write ui port: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().Int("port", defaultUIPort, "TCP port the web dashboard will listen on")
	return cmd
}
