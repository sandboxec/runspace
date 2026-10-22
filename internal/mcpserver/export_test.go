package mcpserver

// OutputCap is the bytes each stream of an MCP result keeps.
const OutputCap = outputCap

// HeartbeatEvery is how often a running command reports progress.
const HeartbeatEvery = heartbeatEvery

// The descriptions that state the output cap or the default timeout.
const (
	ExecDescription = execDescription
	DiffDescription = diffDescription
	CpDescription   = cpDescription
)

// Heartbeat reports progress every HeartbeatEvery until the returned stop is
// called.
func Heartbeat(report func(string)) func() { return heartbeat(report) }

// Tail is the capped writer of one output stream.
type Tail = tail

// NewTail returns an empty Tail.
func NewTail() *Tail { return newTail() }

// The result types, for tests that decode structured content.
type (
	CommandOutput = commandOutput
	RunOutput     = runOutput
	LsOutput      = lsOutput
	DeleteOutput  = deleteOutput
)
