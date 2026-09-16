package cdp

import "os"

// ioPipesFlagPrefix is the Windows-only switch that carries the two inherited
// pipe handle values in the argv. It lives here, untagged, because the spawn path
// writes it and both the Unix and the Windows spawn tests read it: three copies
// of the same literal is one typo away from a test that proves nothing.
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

// pipePair is one direction of the CDP transport. The parent keeps parent and
// hands child to Chromium: on Unix through ExtraFiles, on Windows through an
// inherited handle named in --remote-debugging-io-pipes.
//
// Both ends stay referenced until after cmd.Start. os.NewFile installs a
// finalizer, so a pair whose child end went out of scope early could have its
// descriptor closed by the garbage collector between construction and spawn, and
// Chromium would inherit a closed descriptor.
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
