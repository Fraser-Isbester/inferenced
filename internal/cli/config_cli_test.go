package cli

import (
	"bytes"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// runCLI builds the root command (with the persistent --config flag and every
// registered subcommand), runs it with args, and returns captured output.
func runCLI(args ...string) (string, error) {
	root := rootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

var _ = Describe("config command", func() {
	var configPath string

	BeforeEach(func() {
		// Nest under a subdirectory to exercise Save's parent-dir creation.
		configPath = filepath.Join(GinkgoT().TempDir(), "sub", "inferenced.yaml")
	})

	Describe("init -> validate -> path flow", func() {
		It("writes a starter config, validates it, and reports its path", func() {
			out, err := runCLI("config", "init", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("wrote starter config to"))
			Expect(configPath).To(BeAnExistingFile())

			out, err = runCLI("config", "validate", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("OK"))

			out, err = runCLI("config", "path", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(configPath))
		})
	})

	Describe("init overwrite protection", func() {
		It("refuses to overwrite an existing file without --force", func() {
			_, err := runCLI("config", "init", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())

			_, err = runCLI("config", "init", "--config", configPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already exists"))
		})

		It("overwrites an existing file when --force is given", func() {
			Expect(os.MkdirAll(filepath.Dir(configPath), 0o755)).To(Succeed())
			Expect(os.WriteFile(configPath, []byte("listen: bogus\n"), 0o600)).To(Succeed())

			out, err := runCLI("config", "init", "--config", configPath, "--force")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("wrote starter config to"))

			b, readErr := os.ReadFile(configPath)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(string(b)).To(ContainSubstring("127.0.0.1:8766"))
		})
	})

	Describe("validate", func() {
		It("returns an error for an invalid enforcement action", func() {
			Expect(os.MkdirAll(filepath.Dir(configPath), 0o755)).To(Succeed())
			body := "enforcement:\n  enabled: true\n  action: explode\n"
			Expect(os.WriteFile(configPath, []byte(body), 0o600)).To(Succeed())

			_, err := runCLI("config", "validate", "--config", configPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid enforcement action"))
		})
	})

	Describe("show", func() {
		It("prints the effective configuration as YAML", func() {
			_, err := runCLI("config", "init", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())

			out, err := runCLI("config", "show", "--config", configPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("listen: 127.0.0.1:8766"))
		})
	})
})

var _ = Describe("resolveConfigPath", func() {
	It("returns the flag path when set", func() {
		Expect(resolveConfigPath("/etc/inferenced.yaml")).To(Equal("/etc/inferenced.yaml"))
	})

	It("returns an existing default candidate when no flag is set", func() {
		// The package test directory has no ./inferenced.yaml, so the first
		// existing candidate is the XDG location we create here.
		dir := GinkgoT().TempDir()
		xdg := filepath.Join(dir, "xdg")
		cfg := filepath.Join(xdg, "inferenced", "config.yaml")
		Expect(os.MkdirAll(filepath.Dir(cfg), 0o755)).To(Succeed())
		Expect(os.WriteFile(cfg, []byte("listen: x\n"), 0o600)).To(Succeed())

		prev, had := os.LookupEnv("XDG_CONFIG_HOME")
		Expect(os.Setenv("XDG_CONFIG_HOME", xdg)).To(Succeed())
		DeferCleanup(func() {
			if had {
				Expect(os.Setenv("XDG_CONFIG_HOME", prev)).To(Succeed())
				return
			}
			Expect(os.Unsetenv("XDG_CONFIG_HOME")).To(Succeed())
		})

		Expect(resolveConfigPath("")).To(Equal(cfg))
	})
})
