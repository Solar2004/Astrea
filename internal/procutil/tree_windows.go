//go:build windows

package procutil

import (
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Tree is a started process together with everything it spawns, tracked with
// a Windows Job Object: killing cmd.exe alone would leave llama-server or
// python orphaned, the job kills them all. The job is created with
// KILL_ON_JOB_CLOSE, so if the agent dies its supervised trees die with it
// instead of holding ports that the restarted agent then cannot bind.
type Tree struct{ job windows.Handle }

// PrepareTree is a no-op on Windows; the job is attached after start.
func PrepareTree(*exec.Cmd) {}

// AttachTree assigns the started cmd to a new job object. Children the root
// spawns afterwards inherit the job automatically.
func AttachTree(cmd *exec.Cmd) (*Tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &Tree{job: job}, nil
}

// Kill terminates every process in the tree. Windows has no portable graceful
// signal for console-less processes, so this is immediate.
func (t *Tree) Kill(time.Duration, <-chan struct{}) {
	_ = windows.TerminateJobObject(t.job, 1)
}

// Close releases the job handle (which also kills anything still in it).
func (t *Tree) Close() { _ = windows.CloseHandle(t.job) }
