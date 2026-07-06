// Package reqmeta builds the bounded, structured per-request metadata blob that
// the gateway extracts from coding-agent request bodies for later usage
// analytics. It captures what the agent actually DID — tool-call counts, files
// read vs. edited vs. created, shell commands and the higher-level activity they
// represent (commits, PRs, pushes, tests, builds), tool failures, and cwd/git
// context — never raw prompt or file-content text, and caps unbounded lists so a
// single ledger row cannot grow without limit.
package reqmeta

import "strings"

// maxList caps the number of unique entries kept per file/command list.
const maxList = 200

// Builder accumulates request metadata. It is not safe for concurrent use; a
// Builder is created and consumed within a single ParseRequest call.
type Builder struct {
	toolsAvailable int
	toolCalls      map[string]int
	filesRead      *cappedSet
	filesEdited    *cappedSet
	filesCreated   *cappedSet
	commands       *cappedSet
	edits          int
	toolFailures   int
	cwd            string
	gitRepo        string
	gitBranch      string
	messageCount   int
}

// New returns an empty Builder.
func New() *Builder {
	return &Builder{
		toolCalls:    map[string]int{},
		filesRead:    newCappedSet(maxList),
		filesEdited:  newCappedSet(maxList),
		filesCreated: newCappedSet(maxList),
		commands:     newCappedSet(maxList),
	}
}

// SetToolsAvailable records how many tools the agent advertised (the size of the
// tool catalog), rather than the catalog itself, which is noise for analytics.
func (b *Builder) SetToolsAvailable(n int) { b.toolsAvailable = n }

// AddToolCall increments the count for a tool the model invoked.
func (b *Builder) AddToolCall(name string) {
	if name != "" {
		b.toolCalls[name]++
	}
}

// AddToolUse records one tool invocation: it counts the call and, based on the
// tool's classified operation, routes its input to the right signal (file read/
// edit/create, or a shell command). input is the decoded tool input object.
func (b *Builder) AddToolUse(name string, input map[string]any) {
	b.AddToolCall(name)
	file := firstString(input, "file_path", "path", "notebook_path")
	cmd, hasCmd := input["command"].(string)
	switch classify(name) {
	case opExec:
		if hasCmd {
			b.AddCommand(cmd)
		}
	case opEdit:
		b.AddFileEdit(file)
	case opCreate:
		b.AddFileCreate(file)
	case opRead:
		b.AddFileRead(file)
	default:
		// Unknown tool: only record a command if one is present, to avoid
		// polluting the file lists with search/other-tool inputs.
		if hasCmd {
			b.AddCommand(cmd)
		}
	}
}

// AddFileRead records a file inspected by a read-style tool.
func (b *Builder) AddFileRead(path string) { b.filesRead.add(path) }

// AddFileEdit records a file modified by an edit-style tool and counts the edit
// operation (edits may exceed unique files edited).
func (b *Builder) AddFileEdit(path string) {
	b.edits++
	b.filesEdited.add(path)
}

// AddFileCreate records a file written wholesale by a create/write-style tool.
func (b *Builder) AddFileCreate(path string) { b.filesCreated.add(path) }

// AddCommand records a shell command run by an exec-style tool. Higher-level
// activity (commits, PRs, …) is derived separately by the artifact package,
// which can also carry the resulting link; here we keep the raw command as
// context.
func (b *Builder) AddCommand(cmd string) {
	b.commands.add(cmd)
}

// AddToolResult records the outcome of a tool result; isError increments the
// failure counter (a proxy for how often the agent's actions fail).
func (b *Builder) AddToolResult(isError bool) {
	if isError {
		b.toolFailures++
	}
}

// SetEnv records working-directory / git context parsed from the system prompt.
// Empty values are ignored so a later blank does not clobber an earlier value.
func (b *Builder) SetEnv(cwd, gitRepo, gitBranch string) {
	if cwd != "" {
		b.cwd = cwd
	}
	if gitRepo != "" {
		b.gitRepo = gitRepo
	}
	if gitBranch != "" {
		b.gitBranch = gitBranch
	}
}

// SetMessageCount records the number of messages (turns) in the conversation.
func (b *Builder) SetMessageCount(n int) { b.messageCount = n }

// Map renders the accumulated metadata, omitting empty/zero fields. It returns
// nil when nothing was captured, so callers can leave Event.Metadata empty.
func (b *Builder) Map() map[string]any {
	m := map[string]any{}
	putInt(m, "tools_available", b.toolsAvailable)
	if len(b.toolCalls) > 0 {
		m["tool_call_counts"] = b.toolCalls
	}
	putList(m, "files_read", b.filesRead)
	putList(m, "files_edited", b.filesEdited)
	putList(m, "files_created", b.filesCreated)
	putList(m, "commands", b.commands)
	putInt(m, "edits", b.edits)
	putInt(m, "tool_failures", b.toolFailures)
	putStr(m, "cwd", b.cwd)
	putStr(m, "git_repo", b.gitRepo)
	putStr(m, "git_branch", b.gitBranch)
	putInt(m, "message_count", b.messageCount)
	if len(m) == 0 {
		return nil
	}
	return m
}

func putInt(m map[string]any, k string, v int) {
	if v > 0 {
		m[k] = v
	}
}

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func putList(m map[string]any, k string, s *cappedSet) {
	if items := s.slice(); len(items) > 0 {
		m[k] = items
	}
}

// firstString returns the first non-empty string value among the given keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// op is the classified operation a tool performs.
type op int

const (
	opOther op = iota
	opExec
	opEdit
	opCreate
	opRead
)

// classify maps a tool name to its operation using name heuristics that cover
// Claude Code (Bash/Edit/Write/Read/…) and OpenAI-wire agents (shell/apply_patch/
// read_file/…). Order matters: exec and edit are checked before the broader
// create/read matches (e.g. "NotebookEdit" is an edit, not a read).
func classify(name string) op {
	l := strings.ToLower(name)
	switch {
	case containsAny(l, "bash", "shell", "exec", "terminal"):
		return opExec
	case containsAny(l, "edit", "patch", "str_replace", "replace"):
		return opEdit
	case containsAny(l, "write", "create"):
		return opCreate
	case containsAny(l, "read", "view", "cat", "open", "notebook"):
		return opRead
	default:
		return opOther
	}
}

// cappedSet is an insertion-ordered unique string set with a hard size cap.
type cappedSet struct {
	seen  map[string]bool
	items []string
	cap   int
}

func newCappedSet(capacity int) *cappedSet {
	return &cappedSet{seen: map[string]bool{}, cap: capacity}
}

// add inserts s if it is non-empty, not already present, and the cap is not hit.
func (c *cappedSet) add(s string) {
	if s == "" || c.seen[s] || len(c.items) >= c.cap {
		return
	}
	c.seen[s] = true
	c.items = append(c.items, s)
}

func (c *cappedSet) slice() []string { return c.items }
