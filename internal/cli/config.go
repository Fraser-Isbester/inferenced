package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Fraser-Isbester/inferenced/internal/config"
)

// init registers the config command tree.
func init() { register(newConfigCmd) }

// newConfigCmd builds the "config" parent command and attaches its children:
// init, validate, path, and show.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and manage the inferenced configuration",
	}
	cmd.AddCommand(
		newConfigInitCmd(),
		newConfigValidateCmd(),
		newConfigPathCmd(),
		newConfigShowCmd(),
	)
	return cmd
}

// newConfigInitCmd builds "config init", which writes a starter config to the
// resolved path. It refuses to overwrite an existing file unless --force is set.
func newConfigInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "init",
		Short:        "Write a starter configuration file",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flagPath, _ := cmd.Flags().GetString("config")
			force, _ := cmd.Flags().GetBool("force")
			path := resolveConfigPath(flagPath)

			if !force {
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
				}
			}
			if err := config.Default().Save(path); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote starter config to %s\n", path)
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "overwrite an existing config file")
	return cmd
}

// newConfigValidateCmd builds "config validate", which loads the config and
// verifies its enforcement limits, exiting non-zero on any error.
func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "validate",
		Short:        "Validate the configuration file",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flagPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(flagPath)
			if err != nil {
				return err
			}
			if _, err := cfg.EnforceLimits(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "OK")
			return nil
		},
	}
}

// newConfigPathCmd builds "config path", which prints the path config.Load would
// use: the --config flag when set, else the default search location.
func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path in effect",
		RunE: func(cmd *cobra.Command, _ []string) error {
			flagPath, _ := cmd.Flags().GetString("config")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), resolveConfigPath(flagPath))
			return nil
		},
	}
}

// newConfigShowCmd builds "config show", which loads and prints the effective
// configuration as YAML.
func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "show",
		Short:        "Print the effective configuration as YAML",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flagPath, _ := cmd.Flags().GetString("config")
			cfg, err := config.Load(flagPath)
			if err != nil {
				return err
			}
			b, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
}

// configSearchPaths returns the default config locations in the order that
// config.Load searches them. It mirrors the loader's discovery so that "config
// path" and "config init" agree on the default location.
func configSearchPaths() []string {
	paths := []string{"inferenced.yaml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "inferenced", "config.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "inferenced", "config.yaml"))
	}
	return paths
}

// resolveConfigPath returns the config path in effect. An explicit --config flag
// wins; otherwise the first existing default location is used, falling back to
// the first search candidate when none exist yet.
func resolveConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	candidates := configSearchPaths()
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}
