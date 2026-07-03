// Package cli implements the inferenced command-line interface using cobra.
//
// Commands self-register via register in their init functions, so parallel
// command files never edit a shared list. Execute wires the build version into
// the package-level Version and runs the root command.
package cli

import "github.com/spf13/cobra"

// Version is the build version, set by Execute from the value main injects via
// the -X main.version ldflag. It defaults to "dev" for local builds.
var Version = "dev"

// registry holds the constructors for every subcommand. Each command file adds
// its constructor from an init function via register.
var registry []func() *cobra.Command

// register appends a subcommand constructor to the registry. It is called from
// each command file's init function.
func register(f func() *cobra.Command) { registry = append(registry, f) }

// Execute records the build version and runs the root command. main exits with
// a non-zero status when it returns an error.
func Execute(version string) error {
	Version = version
	return rootCmd().Execute()
}

// rootCmd builds the root command with the persistent --config flag and all
// registered subcommands.
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "inferenced",
		Short: "Local LLM inference gateway: meter, enforce, observe",
	}
	root.PersistentFlags().String("config", "", "config file path (default: ./inferenced.yaml or $XDG_CONFIG_HOME)")
	for _, f := range registry {
		root.AddCommand(f())
	}
	return root
}
