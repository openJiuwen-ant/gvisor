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

package pgalloc

import (
	"bytes"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

func TestCloneBackingFile(t *testing.T) {
	for _, copyOnly := range []bool{false, true} {
		name := "clone"
		if copyOnly {
			name = "copy"
		}
		t.Run(name, func(t *testing.T) {
			src, err := os.CreateTemp(t.TempDir(), "source")
			if err != nil {
				t.Fatal(err)
			}

			mf, err := NewMemoryFile(src, MemoryFileOpts{
				DelayedEviction:         DelayedEvictionDisabled,
				DisableIMAWorkAround:    true,
				DisableMemoryAccounting: true,
				DiskBackedFile:          true,
			})
			if err != nil {
				src.Close()
				t.Fatal(err)
			}
			defer mf.Destroy()

			var ranges []memmap.FileRange
			for _, dir := range []Direction{BottomUp, TopDown} {
				fr, err := mf.Allocate(uint64(hostarch.PageSize), AllocOpts{
					Mode: AllocateCallerIndirectCommit, Dir: dir,
				})
				if err != nil {
					t.Fatal(err)
				}
				defer mf.DecRef(fr)
				ranges = append(ranges, fr)
			}

			data := bytes.Repeat([]byte{0x5a}, int(hostarch.PageSize))
			for _, fr := range ranges {
				if _, err := src.WriteAt(data, int64(fr.Start)); err != nil {
					t.Fatal(err)
				}
			}

			dst, err := os.CreateTemp(t.TempDir(), "destination")
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			if _, err := dst.WriteAt(data, int64(chunkSize/2)); err != nil {
				t.Fatal(err)
			}
			if err := dst.Truncate(2 * chunkSize); err != nil {
				t.Fatal(err)
			}

			if _, err := src.Seek(3, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := dst.Seek(5, io.SeekStart); err != nil {
				t.Fatal(err)
			}

			if !copyOnly && os.Getenv("GVISOR_REQUIRE_REFLINK") == "1" {
				if err := unix.IoctlSetInt(int(dst.Fd()), unix.FICLONE, mf.FD()); err != nil {
					t.Fatalf("test filesystem for %s does not support required reflink: %v", src.Name(), err)
				}
				t.Log("verified FICLONE support on test filesystem")
			}

			if copyOnly {
				if err := dst.Truncate(0); err != nil {
					t.Fatal(err)
				}
				err = mf.copyBackingFile(int(dst.Fd()))
			} else {
				err = mf.CloneBackingFile(int(dst.Fd()))
			}
			if err != nil {
				t.Fatal(err)
			}

			for _, check := range []struct {
				file   *os.File
				offset int64
			}{{src, 3}, {dst, 5}} {
				off, err := check.file.Seek(0, io.SeekCurrent)
				if err != nil || off != check.offset {
					t.Errorf("file offset = %d, %v; want %d", off, err, check.offset)
				}
			}

			got := make([]byte, len(data))
			for _, fr := range ranges {
				if _, err := dst.ReadAt(got, int64(fr.Start)); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("copied allocation at %d differs", fr.Start)
				}
			}

			if _, err := dst.ReadAt(got, int64(chunkSize/2)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, make([]byte, len(got))) {
				t.Error("stale destination data survived in an unallocated range")
			}

			var st unix.Stat_t
			if err := unix.Fstat(int(dst.Fd()), &st); err != nil {
				t.Fatal(err)
			}
			if st.Size != chunkSize {
				t.Errorf("destination size = %d, want %d", st.Size, chunkSize)
			}
			if st.Blocks*512 >= chunkSize/2 {
				t.Errorf("copy populated unused capacity: %d bytes", st.Blocks*512)
			}

			if _, err := dst.WriteAt([]byte{0xff}, int64(ranges[0].Start)); err != nil {
				t.Fatal(err)
			}
			if _, err := src.ReadAt(got, int64(ranges[0].Start)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Error("writing the child changed the source allocation")
			}
		})
	}
}
