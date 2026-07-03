package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// serviceLabel is the reverse-DNS label for the launchd agent and the base name
// (with a .service suffix) for the systemd user unit.
const serviceLabel = "com.fraser-isbester.inferenced"

// systemdUnitName is the systemd user unit filename managed by install/uninstall.
const systemdUnitName = "inferenced.service"

// init registers the install and uninstall commands.
func init() {
	register(newInstallCmd)
	register(newUninstallCmd)
}

// newInstallCmd builds the "install" command, which registers inferenced as a
// background gateway service: a launchd user agent on macOS or a systemd user
// unit on Linux. It is idempotent — running it again reinstalls the current
// binary and reloads the service.
func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "install",
		Short:        "Install the background gateway service (launchd on macOS, systemd user unit on Linux)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installService(cmd.OutOrStdout())
		},
	}
}

// newUninstallCmd builds the "uninstall" command, which stops and removes the
// service unit created by install. It is a no-op (with a notice) when nothing is
// installed.
func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall",
		Short:        "Remove the background gateway service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return uninstallService(cmd.OutOrStdout())
		},
	}
}

// installService resolves the running binary's absolute path and installs the
// per-OS service unit.
func installService(out io.Writer) error {
	binPath, err := resolveBinaryPath()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(out, binPath)
	case "linux":
		return installSystemd(out, binPath)
	default:
		return fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

// uninstallService removes the per-OS service unit.
func uninstallService(out io.Writer) error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(out)
	case "linux":
		return uninstallSystemd(out)
	default:
		return fmt.Errorf("service uninstall is not supported on %s", runtime.GOOS)
	}
}

// resolveBinaryPath returns the absolute, symlink-resolved path of the running
// executable so the service unit points at a stable, unambiguous location.
func resolveBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path of %s: %w", exe, err)
	}
	// Follow symlinks (e.g. a Homebrew shim) to the real binary when possible.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// launchdPlistTemplate is the launchd agent definition. The verbs are: Label,
// ProgramArguments (binary + "serve"), stdout path, stderr path.
const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

// installLaunchd writes the launchd agent plist under ~/Library/LaunchAgents,
// creates the log directory, and (re)loads the agent so it starts now and at
// login. Reinstalling first unloads any prior instance so the new plist takes.
func installLaunchd(out io.Writer, binPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", agentsDir, err)
	}
	logDir := filepath.Join(home, "Library", "Logs", "inferenced")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", logDir, err)
	}

	plistPath := filepath.Join(agentsDir, serviceLabel+".plist")
	stdoutLog := filepath.Join(logDir, "stdout.log")
	stderrLog := filepath.Join(logDir, "stderr.log")
	body := fmt.Sprintf(launchdPlistTemplate, serviceLabel, xmlEscape(binPath), xmlEscape(stdoutLog), xmlEscape(stderrLog))
	if err := os.WriteFile(plistPath, []byte(body), 0o644); err != nil { //nolint:gosec // plist must be user-readable by launchd
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	// Idempotent (re)install: unload any existing instance (ignoring errors when
	// it was never loaded), then load with -w to enable it across logins.
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", "-w", plistPath).Run(); err != nil {
		return fmt.Errorf("launchctl load %s: %w", plistPath, err)
	}

	_, _ = fmt.Fprintf(out, "installed launchd service %s\n", serviceLabel)
	_, _ = fmt.Fprintf(out, "  binary: %s\n", binPath)
	_, _ = fmt.Fprintf(out, "  plist:  %s\n", plistPath)
	_, _ = fmt.Fprintf(out, "  logs:   %s\n", logDir)
	return nil
}

// uninstallLaunchd unloads and removes the launchd agent plist. Missing state is
// reported, not treated as an error.
func uninstallLaunchd(out io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(out, "launchd service %s is not installed\n", serviceLabel)
		return nil
	}

	_ = exec.Command("launchctl", "unload", plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", plistPath, err)
	}

	_, _ = fmt.Fprintf(out, "removed launchd service %s\n", serviceLabel)
	return nil
}

// systemdUnitTemplate is the systemd user unit. stdout/stderr are captured by
// journald (view with: journalctl --user -u inferenced). The single verb is the
// ExecStart binary path.
const systemdUnitTemplate = `[Unit]
Description=inferenced local inference gateway
After=network-online.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// installSystemd writes the systemd user unit under ~/.config/systemd/user,
// reloads the user daemon, and enables + starts the service. It is idempotent:
// daemon-reload and enable --now converge, and an explicit restart picks up a
// changed binary path on reinstall.
func installSystemd(out io.Writer, binPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", unitDir, err)
	}

	unitPath := filepath.Join(unitDir, systemdUnitName)
	body := fmt.Sprintf(systemdUnitTemplate, binPath)
	if err := os.WriteFile(unitPath, []byte(body), 0o644); err != nil { //nolint:gosec // unit must be user-readable by systemd
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	// Idempotent (re)install: reload units, enable + start, then restart so a
	// changed binary path from a prior install is picked up.
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnitName).Run(); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w", systemdUnitName, err)
	}
	_ = exec.Command("systemctl", "--user", "restart", systemdUnitName).Run()

	_, _ = fmt.Fprintf(out, "installed systemd user service %s\n", systemdUnitName)
	_, _ = fmt.Fprintf(out, "  binary: %s\n", binPath)
	_, _ = fmt.Fprintf(out, "  unit:   %s\n", unitPath)
	_, _ = fmt.Fprintf(out, "  logs:   journalctl --user -u %s\n", systemdUnitName)
	return nil
}

// uninstallSystemd stops and disables the service, then removes the unit file and
// reloads the user daemon. Missing state is reported, not treated as an error.
func uninstallSystemd(out io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdUnitName)
	if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(out, "systemd user service %s is not installed\n", systemdUnitName)
		return nil
	}

	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	_, _ = fmt.Fprintf(out, "removed systemd user service %s\n", systemdUnitName)
	return nil
}

// xmlEscaper escapes the XML metacharacters that can appear in a filesystem path
// so plist string values stay well-formed.
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// xmlEscape returns s safe for inclusion in a plist <string> element.
func xmlEscape(s string) string { return xmlEscaper.Replace(s) }
