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

package specutils

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ValidateStdioFile verifies that file is a writable append-only regular file.
func ValidateStdioFile(file *os.File, name string) error {
	return ValidateStdioFileFD(int(file.Fd()), name)
}

// ValidateStdioFileFD verifies that fd is a writable append-only regular file.
func ValidateStdioFileFD(hostFD int, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(hostFD, &stat); err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s must be a regular file", name)
	}
	flags, err := unix.FcntlInt(uintptr(hostFD), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("get %s flags: %w", name, err)
	}
	switch flags & unix.O_ACCMODE {
	case unix.O_WRONLY, unix.O_RDWR:
	default:
		return fmt.Errorf("%s must be opened writable", name)
	}
	if flags&unix.O_APPEND == 0 {
		return fmt.Errorf("%s must be opened with O_APPEND", name)
	}
	return nil
}
