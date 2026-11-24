// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package boot

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/moby/sys/capability"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

//go:nosplit
func _getpid() int {
	pid, _, _ := syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0)
	return int(pid)
}

//go:nosplit
func _close(fd int) {
	syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(fd), 0, 0)
}

//go:nosplit
func _accept(fd int) (int, syscall.Errno) {
	cfd, _, errno := syscall.RawSyscall(syscall.SYS_ACCEPT, uintptr(fd), 0, 0)
	return int(cfd), errno
}

//go:nosplit
func _execve(pathname, argv, envp uintptr) int {
	_, _, errno := syscall.RawSyscall(syscall.SYS_EXECVE, pathname, argv, envp)
	println("execve errno:", errno)
	panic("execve")
}

// cloneCtx carries state across the three nosplit callbacks of
// runtime.Clone. Pinned to a package var so the callbacks can reach it
// without escaping to the heap.
var cloneCtx = &struct {
	argv0              *byte
	argv               []*byte
	cfd                int
	stubFD             int
	loader             *Loader
	finishProcessSetup bool
	lastCap            int
	readyFD            int
}{}

// preclone runs once on both sides of runtime.Clone() before the runtime
// resumes. In the parent branch (pid != 0), execve into the templatestub
// reaper stub so the parent process becomes a minimal init. In the
// child branch (pid == 0), close the memfd used by the parent's execve; the
// child then continues into waiting() to accept the next Fork RPC.
//
//go:nosplit
func preclone(pid int) {
	if pid := _getpid(); pid != 1 {
		println("unexpected PID:", pid, "expected: 1")
		panic(pid)
	}
	if pid != 0 {
		_execve(uintptr(unsafe.Pointer(cloneCtx.argv0)),
			uintptr(unsafe.Pointer(&cloneCtx.argv[0])), 0)
	}
	_close(cloneCtx.stubFD)
	cloneCtx.stubFD = -1
	if cloneCtx.finishProcessSetup {
		if errno := dropTemplateExecCaps(); errno != 0 {
			println("template capability setup errno:", errno)
			syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 1, 0, 0)
			panic("exit_group failed")
		}
	}
}

// templateReady runs in the seed after all workers have been created.
//
//go:nosplit
func templateReady() {
	var ready byte = 1
	for {
		n, _, errno := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(cloneCtx.readyFD), uintptr(unsafe.Pointer(&ready)), 1)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 || n != 1 {
			syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 1, 0, 0)
			panic("template readiness failed")
		}
		break
	}
	_close(cloneCtx.readyFD)
	cloneCtx.readyFD = -1
	_close(cloneCtx.loader.templateServerFD)
}

// waiting runs in each worker with the runtime still stopped. It blocks on
// accept() on the template socket until the next fork RPC arrives, then
// stashes the accepted socket fd on cloneCtx for postclone.
//
//go:nosplit
func waiting() bool {
	if cloneCtx.readyFD >= 0 {
		_close(cloneCtx.readyFD)
		cloneCtx.readyFD = -1
	}
	for {
		cfd, errno := _accept(cloneCtx.loader.templateServerFD)
		if errno == 0 {
			cloneCtx.cfd = cfd
			break
		}
		if errno != syscall.EINTR {
			println("failed to accept:", errno)
			panic("accept")
		}
	}
	return true
}

// postclone runs once on both sides of runtime.Clone() after the
// runtime is stopped. In the parent branch the accepted socket fd
// belongs to the child and we just close our duplicate; in the child
// branch we close the template listening fd (the child is now serving
// the one accepted connection and will not accept more).
//
//go:nosplit
func postclone(pid int) {
	if pid != 0 {
		_close(cloneCtx.cfd)
	} else {
		if pid := _getpid(); pid != 1 {
			println("unexpected PID:", pid, "expected: 1")
			panic(pid)
		}
		_close(cloneCtx.loader.templateServerFD)
		if cloneCtx.finishProcessSetup {
			if errno := finishForkProcessSetup(); errno != 0 {
				println("fork process setup errno:", errno)
				syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 1, 0, 0)
				panic("exit_group failed")
			}
		}
	}
}

var (
	forkProcPath  = [...]byte{'/', 'p', 'r', 'o', 'c', 0}
	forkCapHeader = linux.CapUserHeader{Version: linux.LINUX_CAPABILITY_VERSION_3}
	forkCapData   [2]linux.CapUserData
)

// dropTemplateExecCaps clears capability sets used by exec once, so children
// inherit them empty. The template keeps its effective capabilities to create
// child namespaces and remove procfs.
//
//go:nosplit
func dropTemplateExecCaps() syscall.Errno {
	for c := 0; c <= cloneCtx.lastCap; c++ {
		if _, _, errno := syscall.RawSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0, 0); errno != 0 {
			return errno
		}
	}
	if _, _, errno := syscall.RawSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return 0
}

// finishForkProcessSetup runs in the single-threaded child, before the runtime
// creates threads that must inherit the reduced capability sets.
//
//go:nosplit
func finishForkProcessSetup() syscall.Errno {
	if _, _, errno := syscall.RawSyscall(unix.SYS_UMOUNT2, uintptr(unsafe.Pointer(&forkProcPath[0])), unix.MNT_DETACH, 0); errno != 0 {
		return errno
	}
	_, _, errno := syscall.RawSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&forkCapHeader)), uintptr(unsafe.Pointer(&forkCapData[0])), 0)
	return errno
}

// clone fires runtime.Clone with the template's CLONE_NEW{NS,UTS,PID} flags
// and returns the accepted client socket fd in the child.
func (l *Loader) clone(stubPath string, stubFD int) int {
	var err error

	if cloneCtx.argv0, err = syscall.BytePtrFromString(stubPath); err != nil {
		panic(fmt.Errorf("failed to prepare argv0: %w", err))
	}
	if cloneCtx.argv, err = syscall.SlicePtrFromStrings([]string{
		"templatestub",
	}); err != nil {
		panic(fmt.Errorf("failed to prepare argv: %w", err))
	}
	cloneCtx.stubFD = stubFD
	cloneCtx.loader = l
	cloneCtx.readyFD = l.templateReadyFD
	cloneCtx.finishProcessSetup = !l.root.conf.TestOnlyAllowRunAsCurrentUserWithoutChroot
	if cloneCtx.finishProcessSetup {
		lastCap, err := capability.LastCap()
		if err != nil {
			panic(fmt.Errorf("reading last capability: %w", err))
		}
		cloneCtx.lastCap = int(lastCap)
	}

	defer func() { cloneCtx = nil }()

	// Share the template's host IPC namespace intentionally. CLONE_NEWIPC
	// creates an mqueue superblock, and high sandbox churn can make sget_fc() a
	// latency bottleneck. Guest IPC isolation is implemented by the sentry.
	flags := int(syscall.SIGCHLD | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID)

	runtimeClone(preclone, waiting, postclone, flags, l.templateWorkers, templateReady)

	return cloneCtx.cfd
}
