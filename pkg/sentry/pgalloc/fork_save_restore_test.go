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
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

func newCheckpointMemoryFile(t *testing.T, private bool) *MemoryFile {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "memory")
	if err != nil {
		t.Fatal(err)
	}

	mf, err := NewMemoryFile(file, MemoryFileOpts{
		PrivateAnonymousMapping: private,
		DisableMemoryAccounting: true,
		DisableIMAWorkAround:    true,
		DelayedEviction:         DelayedEvictionDisabled,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(mf.Destroy)
	return mf
}

func TestPrivateAnonymousCheckpoint(t *testing.T) {
	for _, pagesFile := range []bool{false, true} {
		for _, sourcePrivate := range []bool{false, true} {
			for _, destinationPrivate := range []bool{false, true} {
				t.Run(fmt.Sprintf("pagesFile=%t/sourcePrivate=%t/destinationPrivate=%t", pagesFile, sourcePrivate, destinationPrivate), func(t *testing.T) {
					source := newCheckpointMemoryFile(t, sourcePrivate)
					fr, err := source.Allocate(4*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateAndCommit})
					if err != nil {
						t.Fatal(err)
					}
					defer source.DecRef(fr)

					want := make([]byte, fr.Length())
					copy(want[hostarch.PageSize:2*hostarch.PageSize], bytes.Repeat([]byte{0xa5}, int(hostarch.PageSize)))
					source.forEachMappingSlice(fr, func(b []byte) { copy(b, want) })
					source.MarkSavable()

					var snapshot bytes.Buffer
					saveOpts := SaveOpts{}
					var pages *os.File
					done := make(chan error, 1)
					if pagesFile {
						var err error
						pages, err = os.CreateTemp(t.TempDir(), "pages")
						if err != nil {
							t.Fatal(err)
						}
						defer pages.Close()

						pageFD, err := unix.Dup(int(pages.Fd()))
						if err != nil {
							t.Fatal(err)
						}
						saveOpts.PagesFile, err = StartAsyncPagesFileSave(
							stateio.NewPagesFileFDWriterDefault(int32(pageFD)),
							func(err error) { done <- err })
						if err != nil {
							t.Fatal(err)
						}
					}

					saveErr := source.SaveTo(context.Background(), &snapshot, &saveOpts)
					if saveOpts.PagesFile != nil {
						saveOpts.PagesFile.MemoryFilesDone()
						if err := <-done; err != nil {
							t.Fatal(err)
						}
					}
					if saveErr != nil {
						t.Fatal(saveErr)
					}

					destination := newCheckpointMemoryFile(t, destinationPrivate)
					loadOpts := LoadOpts{}
					if pagesFile {
						pageFD, err := unix.Dup(int(pages.Fd()))
						if err != nil {
							t.Fatal(err)
						}
						loadOpts.PagesFile, err = StartAsyncPagesFileLoad(
							stateio.NewPagesFileFDReaderDefault(int32(pageFD), 2 /* numCPUs */),
							func(err error) { done <- err }, nil, AsyncPagesFileLoadOpts{})
						if err != nil {
							t.Fatal(err)
						}
					}

					loadErr := destination.LoadFrom(context.Background(), &snapshot, &loadOpts)
					if loadOpts.PagesFile != nil {
						loadOpts.PagesFile.MemoryFilesDone()
						if err := <-done; err != nil {
							t.Fatal(err)
						}
					}
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					defer destination.DecRef(fr)

					destination.forEachMappingSlice(fr, func(b []byte) {
						if !bytes.Equal(b, want) {
							t.Error("restored pages differ from the saved data")
						}
					})
					if destinationPrivate {
						st, err := destination.file.Stat()
						if err != nil || st.Size() != 0 {
							t.Fatalf("private restore grew identity file: %v, %v", st, err)
						}
					}

					if err := destination.decommitFile(fr); err != nil {
						t.Fatal(err)
					}
					destination.forEachMappingSlice(fr, func(b []byte) {
						if !bytes.Equal(b, make([]byte, len(b))) {
							t.Error("decommitted restored pages are not zero")
						}
					})

					source.forEachMappingSlice(fr, func(b []byte) {
						if !bytes.Equal(b, want) {
							t.Error("restored memory aliases the source")
						}
					})
				})
			}
		}
	}
}

type fileOnlyPageWriter struct {
	stateio.AsyncWriter
}

func (fileOnlyPageWriter) NeedRegisterSourceFD() bool { return true }

type fileOnlyPageReader struct {
	stateio.AsyncReader
}

func (fileOnlyPageReader) NeedRegisterDestinationFD() bool { return true }

func TestPrivateAnonymousCheckpointRequiresBackingFile(t *testing.T) {
	mf := newCheckpointMemoryFile(t, true)
	var data bytes.Buffer

	saveOpts := SaveOpts{PagesFile: &AsyncPagesFileSave{aw: fileOnlyPageWriter{}}}
	if err := mf.SaveTo(context.Background(), &data, &saveOpts); !errors.Is(err, linuxerr.EOPNOTSUPP) {
		t.Fatalf("SaveTo = %v, want EOPNOTSUPP", err)
	}

	callbackCalled := false
	loadOpts := LoadOpts{
		PagesFile: &AsyncPagesFileLoad{ar: fileOnlyPageReader{}},
		DoneCallback: func(err error) {
			callbackCalled = true
			if !errors.Is(err, linuxerr.EOPNOTSUPP) {
				t.Errorf("load callback error = %v, want EOPNOTSUPP", err)
			}
		},
	}
	if err := mf.LoadFrom(context.Background(), &data, &loadOpts); !errors.Is(err, linuxerr.EOPNOTSUPP) {
		t.Fatalf("LoadFrom = %v, want EOPNOTSUPP", err)
	}
	if !callbackCalled {
		t.Error("load failure did not invoke DoneCallback")
	}
}
