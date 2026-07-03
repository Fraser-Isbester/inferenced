// Package reqmeta builds the bounded, structured per-request metadata blob that
// the gateway extracts from coding-agent request bodies for later usage
// analytics. It captures signals (tool names, tool-call counts, file paths,
// shell commands, working directory / git repo) — never raw prompt or
// file-content text — and caps unbounded lists so a single ledger row cannot
// grow without limit.
package reqmeta

// maxList caps the number of unique entries kept for files and commands, so a
// very long conversation cannot produce an unbounded metadata blob.
const maxList = 200

// Builder accumulates request metadata. It is not safe for concurrent use; a
// Builder is created and consumed within a single ParseRequest call.
type Builder struct {
	tools        []string
	toolSeen     map[string]bool
	toolCalls    map[string]int
	files        *cappedSet
	commands     *cappedSet
	cwd          string
	gitRepo      string
	gitBranch    string
	messageCount int
}

// New returns an empty Builder.
func New() *Builder {
	return &Builder{
		toolSeen:  map[string]bool{},
		toolCalls: map[string]int{},
		files:     newCappedSet(maxList),
		commands:  newCappedSet(maxList),
	}
}

// AddTool records a tool the agent advertised as available.
func (b *Builder) AddTool(name string) {
	if name == "" || b.toolSeen[name] {
		return
	}
	b.toolSeen[name] = true
	b.tools = append(b.tools, name)
}

// AddToolCall increments the count for a tool the model invoked.
func (b *Builder) AddToolCall(name string) {
	if name == "" {
		return
	}
	b.toolCalls[name]++
}

// AddFile records a file path touched by a tool call.
func (b *Builder) AddFile(path string) { b.files.add(path) }

// AddCommand records a shell command run by a tool call (captures git/gh, etc.).
func (b *Builder) AddCommand(cmd string) { b.commands.add(cmd) }

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

// Map renders the accumulated metadata, omitting empty fields. It returns nil
// when nothing was captured, so callers can leave Event.Metadata empty.
func (b *Builder) Map() map[string]any {
	m := map[string]any{}
	if len(b.tools) > 0 {
		m["tools"] = b.tools
	}
	if len(b.toolCalls) > 0 {
		m["tool_call_counts"] = b.toolCalls
	}
	if files := b.files.slice(); len(files) > 0 {
		m["files"] = files
	}
	if cmds := b.commands.slice(); len(cmds) > 0 {
		m["commands"] = cmds
	}
	if b.cwd != "" {
		m["cwd"] = b.cwd
	}
	if b.gitRepo != "" {
		m["git_repo"] = b.gitRepo
	}
	if b.gitBranch != "" {
		m["git_branch"] = b.gitBranch
	}
	if b.messageCount > 0 {
		m["message_count"] = b.messageCount
	}
	if len(m) == 0 {
		return nil
	}
	return m
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
