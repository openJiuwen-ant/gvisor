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

// Package templatestub provides a minimal static C binary that acts as the
// reaper process for the template sandbox's process tree. After
// runtime.Clone(), the template parent execve's into this stub instead of
// /proc/self/exe, avoiding the Go runtime's sysMmap calls that trigger
// SIGSYS from the inherited seccomp mmap filter.
package templatestub

import (
	_ "embed"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

//go:embed templatestub.bin
var binary []byte

// PrepareMemfd writes the embedded stub binary to a sealed memfd. It returns
// the memfd's /proc/self/fd path and fd for use by runtime.Clone callbacks.
func PrepareMemfd() (string, int, error) {
	flags := unix.MFD_ALLOW_SEALING | unix.MFD_CLOEXEC
	fd, err := unix.MemfdCreate("templatestub", flags|unix.MFD_EXEC)
	if err == unix.EINVAL {
		fd, err = unix.MemfdCreate("templatestub", flags)
	}
	if err != nil {
		return "", -1, fmt.Errorf("memfd_create: %w", err)
	}
	if err := writeAll(fd, binary); err != nil {
		unix.Close(fd)
		return "", -1, fmt.Errorf("write stub to memfd: %w", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS,
		unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		unix.Close(fd)
		return "", -1, fmt.Errorf("seal memfd: %w", err)
	}
	return fmt.Sprintf("/proc/self/fd/%d", fd), fd, nil
}

func writeAll(fd int, data []byte) error {
	for len(data) != 0 {
		n, err := unix.Write(fd, data)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
