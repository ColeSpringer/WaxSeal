package cdp

import "os"

// ioPipesFlagPrefix is the Windows-only switch that carries the two inherited
// pipe handle values in the argv. It is untagged so the Unix spawn test checks
// the same literal the Windows spawn path writes.
const ioPipesFlagPrefix = "--remote-debugging-io-pipes="

// pipeDir names which side of a pipe the parent holds.
type pipeDir int

const (
	// pipeParentWrites carries commands: the parent writes, the child reads.
	pipeParentWrites pipeDir = iota
	// pipeParentReads carries responses and events: the child writes, the parent
	// reads.
	pipeParentReads
)

// pipePair is one direction of the CDP transport: the parent keeps parent, and
// procGuard.attach has Chromium inherit child. The pair holds child until after
// cmd.Start so the *os.File finalizer cannot close it first and hand Chromium a
// closed descriptor.
type pipePair struct {
	parent *os.File
	child  *os.File
}

// close releases both ends. It is safe on a partially built pair.
func (p *pipePair) close() {
	if p == nil {
		return
	}
	if p.parent != nil {
		_ = p.parent.Close()
	}
	if p.child != nil {
		_ = p.child.Close()
	}
}

// closeChild releases the parent's copy of the child end after the spawn. A
// lingering copy of the response pipe's write end would keep that pipe from ever
// reaching EOF when Chromium exits, which is the transport's own death signal.
func (p *pipePair) closeChild() {
	if p == nil || p.child == nil {
		return
	}
	_ = p.child.Close()
	p.child = nil
}

// newPipePair builds one direction of the transport. childPollable asks for a
// child end the current process can also give deadlines to, which only the
// transport tests need: production hands that end to Chromium, which does its own
// blocking I/O on it.
func newPipePair(dir pipeDir, childPollable bool) (*pipePair, error) {
	return newPlatformPipePair(dir, childPollable)
}
