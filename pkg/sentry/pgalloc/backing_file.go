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
	"fmt"
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// CloneBackingFile replaces dstFD's contents with the live contents of f's
// backing file. It uses reflink when available, otherwise copies allocated
// ranges. Neither file offset is changed.
//
// Preconditions: f is file-backed and its users are quiescent. dstFD is a
// writable regular file distinct from f's backing file.
func (f *MemoryFile) CloneBackingFile(dstFD int) error {
	if err := unix.Ftruncate(dstFD, 0); err != nil {
		return fmt.Errorf("truncate destination backend file: %w", err)
	}

	switch err := unix.IoctlSetInt(dstFD, unix.FICLONE, f.FD()); err {
	case nil:
		return nil
	case unix.EINVAL, unix.ENOTTY, unix.EOPNOTSUPP, unix.EXDEV:
		return f.copyBackingFile(dstFD)
	default:
		return err
	}
}

// copyBackingFile copies live allocations without populating unused capacity.
// MemoryFile grows in GiB chunks, so copying its full size can dwarf the amount
// of data held by a template. dstFD must be empty.
func (f *MemoryFile) copyBackingFile(dstFD int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var st unix.Stat_t
	if err := unix.Fstat(f.FD(), &st); err != nil {
		return fmt.Errorf("stat source backend file: %w", err)
	}
	if err := unix.Ftruncate(dstFD, st.Size); err != nil {
		return fmt.Errorf("resize destination backend file: %w", err)
	}

	buf := make([]byte, 1<<20)
	for _, ranges := range []*unfreeSet{&f.unfreeSmall, &f.unfreeHuge} {
		for seg := ranges.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if seg.Value().refs == 0 {
				continue
			}
			if err := copyBackingFileRange(dstFD, f.FD(), seg.Range(), buf); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyBackingFileRange(dstFD, srcFD int, fr memmap.FileRange, buf []byte) error {
	for off := int64(fr.Start); off < int64(fr.End); {
		want := min(int64(fr.End)-off, int64(len(buf)))
		n, err := unix.Pread(srcFD, buf[:int(want)], off)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("read source backend file at offset %d: %w", off, err)
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}

		for written := 0; written < n; {
			w, err := unix.Pwrite(dstFD, buf[written:n], off+int64(written))
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return fmt.Errorf("write destination backend file at offset %d: %w", off+int64(written), err)
			}
			if w == 0 {
				return io.ErrShortWrite
			}
			written += w
		}
		off += int64(n)
	}
	return nil
}
