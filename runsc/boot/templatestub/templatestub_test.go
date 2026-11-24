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

package templatestub

import (
	"bytes"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrepareMemfd(t *testing.T) {
	path, fd, err := PrepareMemfd()
	if err != nil {
		t.Fatalf("PrepareMemfd: %v", err)
	}
	defer unix.Close(fd)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("memfd contains %d bytes, want embedded binary with %d bytes", len(got), len(binary))
	}

	fdFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if fdFlags&unix.FD_CLOEXEC == 0 {
		t.Errorf("memfd is missing FD_CLOEXEC")
	}

	seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatalf("F_GET_SEALS: %v", err)
	}
	const wantSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if seals&wantSeals != wantSeals {
		t.Errorf("memfd seals = %#x, want at least %#x", seals, wantSeals)
	}
}
