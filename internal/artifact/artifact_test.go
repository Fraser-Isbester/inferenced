package artifact_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/artifact"
)

func TestArtifact(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Artifact Suite")
}

func bash(cmd, result string) artifact.Interaction {
	return artifact.Interaction{ToolName: "Bash", Input: map[string]any{"command": cmd}, Result: result}
}

var _ = Describe("Extract", func() {
	It("extracts a created GitHub PR with its link from the result", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("gh pr create --fill", "https://github.com/acme/app/pull/42\n"),
		})
		Expect(got).To(ConsistOf(artifact.Artifact{
			Kind: "pull_request", Source: "github",
			URL: "https://github.com/acme/app/pull/42", Ref: "#42",
		}))
	})

	It("treats GitLab MRs and Bitbucket PRs as the same abstract kind", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("glab mr create", "https://gitlab.example.com/g/p/-/merge_requests/7"),
			bash("… pull-request create", "https://bitbucket.org/g/p/pull-requests/9"),
		})
		Expect(got).To(ContainElement(artifact.Artifact{Kind: "pull_request", Source: "gitlab", URL: "https://gitlab.example.com/g/p/-/merge_requests/7", Ref: "#7"}))
		Expect(got).To(ContainElement(artifact.Artifact{Kind: "pull_request", Source: "bitbucket", URL: "https://bitbucket.org/g/p/pull-requests/9", Ref: "#9"}))
	})

	It("does NOT record a PR that was only viewed, not created", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("gh pr view 42", "https://github.com/acme/app/pull/42"),
		})
		Expect(got).To(BeEmpty())
	})

	It("extracts a commit sha + branch from git commit output", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("git commit -m wip", "[main a1b2c3d] wip\n 1 file changed"),
		})
		Expect(got).To(ConsistOf(artifact.Artifact{Kind: "commit", Source: "git", Ref: "a1b2c3d", Title: "main"}))
	})

	It("extracts branch creation and Jira issues by key", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("git checkout -b feature/x", ""),
			bash("… issue create", "https://acme.atlassian.net/browse/PROJ-123"),
		})
		Expect(got).To(ContainElement(artifact.Artifact{Kind: "branch", Source: "git", Ref: "feature/x"}))
		Expect(got).To(ContainElement(artifact.Artifact{Kind: "issue", Source: "jira", URL: "https://acme.atlassian.net/browse/PROJ-123", Ref: "PROJ-123"}))
	})

	It("dedupes the same artifact seen across turns", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("gh pr create", "https://github.com/acme/app/pull/42"),
			bash("gh pr create", "https://github.com/acme/app/pull/42"),
		})
		Expect(got).To(HaveLen(1))
	})

	It("returns nothing for ordinary tool calls", func() {
		got := artifact.Extract([]artifact.Interaction{
			bash("go test ./...", "ok"),
			{ToolName: "Read", Input: map[string]any{"file_path": "main.go"}},
		})
		Expect(got).To(BeEmpty())
	})
})
