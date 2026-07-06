// Package artifact performs second-order analysis over an agent's tool calls to
// extract linkable business outcomes — pull/merge requests, commits, pushes,
// branches, issues, releases — as a forge-neutral model. Matchers recognize
// specific tools/forges (GitHub, GitLab, Bitbucket, Jira, git), but the produced
// Artifact is abstract: a Kind + Source + URL/Ref, so downstream analytics never
// hard-code a vendor.
//
// Extraction is command-gated and reads the link from the tool RESULT, so it
// records what the agent PRODUCED (e.g. `gh pr create` printing a new PR URL),
// not what it merely referenced (`gh pr view`).
package artifact

import (
	"regexp"
	"strings"
)

// Kinds are the abstract, forge-neutral artifact types.
const (
	KindPullRequest = "pull_request" // GitHub/GitLab/Bitbucket PR or MR
	KindCommit      = "commit"
	KindPush        = "push"
	KindBranch      = "branch"
	KindIssue       = "issue"
	KindRelease     = "release"
)

// Artifact is a single business outcome produced during a request.
type Artifact struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	URL    string `json:"url,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Title  string `json:"title,omitempty"`
}

// Interaction is one tool call paired with its result text, the input to
// extraction. The command (for shell tools) is read from Input["command"].
type Interaction struct {
	ToolName string
	Input    map[string]any
	Result   string
}

var (
	// URL shapes across forges. Kept deliberately permissive on host so
	// self-hosted GitLab/Bitbucket still match.
	prURLRe      = regexp.MustCompile(`https?://[^\s"']*/(?:pull/(\d+)|-/merge_requests/(\d+)|pull-requests/(\d+))`)
	issueURLRe   = regexp.MustCompile(`https?://[^\s"']*/(?:issues/(\d+)|-/issues/(\d+)|browse/([A-Z][A-Z0-9]+-\d+))`)
	releaseURLRe = regexp.MustCompile(`https?://[^\s"']*/releases/tag/([^\s"']+)`)

	// git command output shapes.
	commitRe = regexp.MustCompile(`\[(\S+)\s+([0-9a-f]{7,40})\]`)          // [main a1b2c3d] msg
	branchRe = regexp.MustCompile(`(?:checkout\s+-b|switch\s+-c)\s+(\S+)`) // new-branch creation
	pushRe   = regexp.MustCompile(`git\s+push(?:\s+\S+\s+(\S+))?`)         // remote branch (optional)
)

// Extract returns the deduplicated artifacts produced across the interactions.
func Extract(ins []Interaction) []Artifact {
	var out []Artifact
	seen := map[string]bool{}
	add := func(a Artifact) {
		if DedupKey(a) == "" || seen[DedupKey(a)] {
			return
		}
		seen[DedupKey(a)] = true
		out = append(out, a)
	}

	for _, in := range ins {
		cmd, _ := in.Input["command"].(string)
		lc := strings.ToLower(cmd)
		res := in.Result

		switch {
		case containsAny(lc, "pr create", "mr create", "merge-request create", "pull-request create"):
			if u := firstURL(prURLRe, res); u != "" {
				add(Artifact{Kind: KindPullRequest, Source: sourceOf(u), URL: u, Ref: prRef(u)})
			}
		case containsAny(lc, "issue create"):
			if u := firstURL(issueURLRe, res); u != "" {
				add(Artifact{Kind: KindIssue, Source: sourceOf(u), URL: u, Ref: issueRef(u)})
			}
		case containsAny(lc, "release create"):
			if u := firstURL(releaseURLRe, res); u != "" {
				m := releaseURLRe.FindStringSubmatch(u)
				add(Artifact{Kind: KindRelease, Source: sourceOf(u), URL: u, Ref: m[1]})
			}
		}

		if strings.Contains(lc, "git commit") {
			if m := commitRe.FindStringSubmatch(res); m != nil {
				add(Artifact{Kind: KindCommit, Source: "git", Ref: m[2], Title: m[1]})
			}
		}
		if strings.Contains(lc, "git push") {
			if m := pushRe.FindStringSubmatch(lc); m != nil && m[1] != "" {
				add(Artifact{Kind: KindPush, Source: "git", Ref: m[1]})
			}
		}
		if m := branchRe.FindStringSubmatch(lc); m != nil {
			add(Artifact{Kind: KindBranch, Source: "git", Ref: m[1]})
		}
	}
	return out
}

// DedupKey identifies an artifact for de-duplication (both within a request and
// in the persistent store): its URL when present, else kind+source+ref. Returns
// "" when neither is available (unidentifiable, and thus not persisted).
func DedupKey(a Artifact) string {
	if a.URL != "" {
		return a.URL
	}
	if a.Ref != "" {
		return a.Kind + ":" + a.Source + ":" + a.Ref
	}
	return ""
}

// sourceOf maps a URL to its forge/source.
func sourceOf(u string) string {
	l := strings.ToLower(u)
	switch {
	case strings.Contains(l, "github.com"):
		return "github"
	case strings.Contains(l, "gitlab"):
		return "gitlab"
	case strings.Contains(l, "bitbucket"):
		return "bitbucket"
	case strings.Contains(l, "atlassian.net"), strings.Contains(l, "/browse/"):
		return "jira"
	default:
		return "git"
	}
}

// prRef returns the PR/MR number as a "#N" ref.
func prRef(u string) string {
	m := prURLRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return "#" + g
		}
	}
	return ""
}

// issueRef returns the issue number ("#N") or tracker key (e.g. PROJ-123).
func issueRef(u string) string {
	m := issueURLRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return "#" + m[1]
	}
	if m[2] != "" {
		return "#" + m[2]
	}
	return m[3]
}

func firstURL(re *regexp.Regexp, text string) string {
	return re.FindString(text)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
