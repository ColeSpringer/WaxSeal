//go:build windows

package cdp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// CreatePipe's handles are synchronous: a parked read takes no deadline and
// cannot be cancelled by closing from another goroutine. An overlapped named pipe
// can, so that is what the parent holds; the child gets a synchronous end, which
// is what Chromium expects. syscall exports none of these calls, so they come
// from kernel32 and the module stays standard-library only.
var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateNamedPipeW     = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe     = kernel32.NewProc("ConnectNamedPipe")
	procCreateEventW         = kernel32.NewProc("CreateEventW")
	procGetHandleInformation = kernel32.NewProc("GetHandleInformation")
)

const (
	pipeAccessInbound         = 0x00000001
	pipeAccessOutbound        = 0x00000002
	pipeTypeByte              = 0x00000000
	pipeReadmodeByte          = 0x00000000
	pipeWaitMode              = 0x00000000
	pipeRejectRemoteClients   = 0x00000008
	fileFlagFirstPipeInstance = 0x00080000

	// errorPipeConnected is what ConnectNamedPipe reports when the client
	// attached before the call, which is the expected outcome here.
	errorPipeConnected = syscall.Errno(535)

	// pipeBufferBytes sizes each direction's kernel buffer. CDP frames are small;
	// this is generous so a burst of events cannot stall Chromium's writer.
	pipeBufferBytes = 64 << 10

	// overlappedDrainTimeout bounds the wait for a cancelled ConnectNamedPipe to
	// report completion, after which its Overlapped is pinned rather than freed.
	overlappedDrainTimeout = 5 * time.Second
)

// pinnedOverlapped holds every Overlapped whose operation never reported
// completion. The kernel may still write to that memory, so it is never freed.
var (
	pinnedMu         sync.Mutex
	pinnedOverlapped []*syscall.Overlapped
)

func pinOverlapped(ov *syscall.Overlapped) {
	pinnedMu.Lock()
	pinnedOverlapped = append(pinnedOverlapped, ov)
	pinnedMu.Unlock()
}

// newPlatformPipePair creates a one-instance named pipe and connects a client to
// it, returning the overlapped server end as parent and the inheritable client
// end as child. The name carries the pid and 128 random bits so two pairs cannot
// collide; FIRST_PIPE_INSTANCE turns a name that somehow exists into an error
// rather than someone else's pipe, and REJECT_REMOTE_CLIENTS keeps it local.
// childPollable is for the transport tests, which need deadlines on the far end.
func newPlatformPipePair(dir pipeDir, childPollable bool) (_ *pipePair, err error) {
	var seed [16]byte
	if _, rerr := rand.Read(seed[:]); rerr != nil {
		return nil, fmt.Errorf("cdp: pipe name entropy: %w", rerr)
	}
	name := fmt.Sprintf(`\\.\pipe\waxseal-%d-%s`, os.Getpid(), hex.EncodeToString(seed[:]))
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("cdp: pipe name: %w", err)
	}

	// The server end's access is the parent's direction; the client end's is the
	// child's, which is the opposite.
	serverAccess, clientAccess := uint32(pipeAccessOutbound), uint32(syscall.GENERIC_READ)
	if dir == pipeParentReads {
		serverAccess, clientAccess = pipeAccessInbound, syscall.GENERIC_WRITE
	}

	serverHandle, _, callErr := procCreateNamedPipeW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(serverAccess|syscall.FILE_FLAG_OVERLAPPED|fileFlagFirstPipeInstance),
		uintptr(pipeTypeByte|pipeReadmodeByte|pipeWaitMode|pipeRejectRemoteClients),
		1, // nMaxInstances: exactly one client, this child
		pipeBufferBytes,
		pipeBufferBytes,
		0, // default timeout
		0, // no SECURITY_ATTRIBUTES: the server end is not inheritable
	)
	if syscall.Handle(serverHandle) == syscall.InvalidHandle {
		return nil, fmt.Errorf("cdp: CreateNamedPipeW: %w", callErr)
	}
	// Zeroed once an *os.File takes ownership, so these defers close each handle
	// exactly once. A double close would land on whatever handle Windows has since
	// given that value to.
	server := syscall.Handle(serverHandle)
	defer func() {
		if err != nil && server != 0 {
			_ = syscall.CloseHandle(server)
		}
	}()

	// Only the client end is inheritable, so Chromium receives exactly the two
	// handles it is told about and nothing else leaks into it.
	sa := &syscall.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	clientFlags := uint32(0)
	if childPollable {
		clientFlags = syscall.FILE_FLAG_OVERLAPPED
	}
	client, cerr := syscall.CreateFile(namePtr, clientAccess, 0, sa, syscall.OPEN_EXISTING, clientFlags, 0)
	if cerr != nil {
		return nil, fmt.Errorf("cdp: open pipe client end: %w", cerr)
	}
	defer func() {
		if err != nil && client != 0 {
			_ = syscall.CloseHandle(client)
		}
	}()

	if err = connectServerEnd(server); err != nil {
		return nil, err
	}

	parent := os.NewFile(uintptr(server), name)
	if parent == nil {
		err = fmt.Errorf("cdp: os.NewFile on the pipe server end returned nil")
		return nil, err
	}
	server = 0 // parent owns it now
	defer func() {
		if err != nil {
			_ = parent.Close()
		}
	}()
	// os.NewFile swallows a failed poller association, leaving a file whose
	// deadlines silently do nothing. Prove it here rather than discover it during
	// a stall; a zero time clears the deadline, so the probe leaves no trace.
	if derr := parent.SetReadDeadline(time.Time{}); derr != nil {
		err = fmt.Errorf("cdp: pipe server end is not pollable (deadlines and cancel-on-close would not work): %w", derr)
		return nil, err
	}

	childFile := os.NewFile(uintptr(client), name+"-child")
	if childFile == nil {
		err = fmt.Errorf("cdp: os.NewFile on the pipe client end returned nil")
		return nil, err
	}
	client = 0 // childFile owns it now
	return &pipePair{parent: parent, child: childFile}, nil
}

// connectServerEnd completes the server side. The client is already attached, so
// ERROR_PIPE_CONNECTED is the expected result and TRUE is accepted too. The
// overlapped end needs a real event or the kernel has nowhere to signal
// completion. ERROR_IO_PENDING is still an error here, but it cannot just return:
// the kernel owns ov, which is Go heap the collector may reuse, so the operation
// is cancelled and drained first, and pinned if the drain does not complete.
func connectServerEnd(server syscall.Handle) error {
	event, _, eerr := procCreateEventW.Call(0, 1, 0, 0) // manual reset, initially unsignaled
	if event == 0 {
		return fmt.Errorf("cdp: CreateEventW: %w", eerr)
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(event)) }()

	ov := &syscall.Overlapped{HEvent: syscall.Handle(event)}
	ret, _, cerr := procConnectNamedPipe.Call(uintptr(server), uintptr(unsafe.Pointer(ov)))
	if ret != 0 || cerr == errorPipeConnected {
		return nil
	}
	if cerr == syscall.ERROR_IO_PENDING {
		// A cancelled ConnectNamedPipe completes at once, and CancelIoEx failing
		// with ERROR_NOT_FOUND means the operation already finished and the event
		// is already signaled. The wait is bounded all the same: should the
		// completion never arrive, or the wait itself fail, ov is pinned for the
		// life of the process rather than returned to a collector that would hand
		// the kernel's target to something else.
		_ = syscall.CancelIoEx(server, ov)
		ev, werr := syscall.WaitForSingleObject(syscall.Handle(event), uint32(overlappedDrainTimeout/time.Millisecond))
		if ev != syscall.WAIT_OBJECT_0 {
			pinOverlapped(ov)
			return fmt.Errorf("cdp: ConnectNamedPipe: no client was attached, and the cancel did not complete (wait %#x, %v): %w", ev, werr, cerr)
		}
		runtime.KeepAlive(ov)
		return fmt.Errorf("cdp: ConnectNamedPipe: no client was attached: %w", cerr)
	}
	return fmt.Errorf("cdp: ConnectNamedPipe: %w", cerr)
}

// handleIsInheritable reports whether h carries HANDLE_FLAG_INHERIT. The tests
// use it to prove the child end is inheritable and the parent end is not.
func handleIsInheritable(h syscall.Handle) (bool, error) {
	var flags uint32
	ret, _, err := syscall.SyscallN(procGetHandleInformation.Addr(), uintptr(h), uintptr(unsafe.Pointer(&flags)))
	if ret == 0 {
		return false, fmt.Errorf("cdp: GetHandleInformation: %w", err)
	}
	return flags&syscall.HANDLE_FLAG_INHERIT != 0, nil
}
