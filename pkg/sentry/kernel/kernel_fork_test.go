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

package kernel

import (
	"bytes"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

func TestUpdateMemoryFileFilestoreRemapsExistingChunks(t *testing.T) {
	src, err := os.CreateTemp(t.TempDir(), "src")
	if err != nil {
		t.Fatalf("CreateTemp(src): %v", err)
	}

	mf, err := pgalloc.NewMemoryFile(src, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		DiskBackedFile:          true,
	})
	if err != nil {
		src.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	defer mf.Destroy()

	fr, err := mf.Allocate(uint64(hostarch.PageSize), pgalloc.AllocOpts{
		Mode: pgalloc.AllocateCallerIndirectCommit,
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer mf.DecRef(fr)

	templateData := []byte("tmpl-data")
	if n, err := unix.Pwrite(mf.FD(), templateData, int64(fr.Start)); err != nil {
		t.Fatalf("Pwrite(template): %v", err)
	} else if n != len(templateData) {
		t.Fatalf("Pwrite(template) wrote %d bytes, want %d", n, len(templateData))
	}

	templateFD, err := unix.Dup(mf.FD())
	if err != nil {
		t.Fatalf("Dup(template backend): %v", err)
	}
	templateFile := os.NewFile(uintptr(templateFD), "template-backend")
	defer templateFile.Close()

	childFile, err := os.CreateTemp(t.TempDir(), "child")
	if err != nil {
		t.Fatalf("CreateTemp(child): %v", err)
	}
	defer childFile.Close()

	k := Kernel{
		fileBackedTmpfsMFs: map[string]*pgalloc.MemoryFile{"/": mf},
	}
	if err := k.UpdateMemoryFileFilestore(map[string]*os.File{"/": childFile}); err != nil {
		t.Fatalf("UpdateMemoryFileFilestore: %v", err)
	}

	blocks, err := mf.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	got := blocks.Head().ToSlice()[:len(templateData)]
	if !bytes.Equal(got, templateData) {
		t.Fatalf("remapped contents = %q, want %q", got, templateData)
	}

	childData := []byte("child-dat")
	copy(got, childData)

	readAt := func(name string, file *os.File) []byte {
		t.Helper()
		buf := make([]byte, len(templateData))
		if n, err := file.ReadAt(buf, int64(fr.Start)); err != nil {
			t.Fatalf("ReadAt(%s): %v", name, err)
		} else if n != len(buf) {
			t.Fatalf("ReadAt(%s) read %d bytes, want %d", name, n, len(buf))
		}
		return buf
	}

	if got := readAt("template", templateFile); !bytes.Equal(got, templateData) {
		t.Errorf("template contents = %q, want %q", got, templateData)
	}
	if got := readAt("child", childFile); !bytes.Equal(got, childData) {
		t.Errorf("child contents = %q, want %q", got, childData)
	}
}

func TestUpdateMemoryFileFilestoreRejectsTemplateAlias(t *testing.T) {
	src, err := os.CreateTemp(t.TempDir(), "template")
	if err != nil {
		t.Fatal(err)
	}

	mf, err := pgalloc.NewMemoryFile(src, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		DiskBackedFile:          true,
	})
	if err != nil {
		src.Close()
		t.Fatal(err)
	}
	defer mf.Destroy()

	data := []byte("template must remain unchanged")
	if _, err := unix.Pwrite(mf.FD(), data, 0); err != nil {
		t.Fatal(err)
	}

	aliasFD, err := unix.Dup(mf.FD())
	if err != nil {
		t.Fatal(err)
	}
	alias := os.NewFile(uintptr(aliasFD), "alias")
	defer alias.Close()

	k := Kernel{fileBackedTmpfsMFs: map[string]*pgalloc.MemoryFile{"/": mf}}
	if err := k.UpdateMemoryFileFilestore(map[string]*os.File{"/": alias}); err == nil {
		t.Fatal("accepted a duplicate of the template backing file")
	}

	got := make([]byte, len(data))
	if _, err := unix.Pread(mf.FD(), got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("template contents changed: %q", got)
	}
}
