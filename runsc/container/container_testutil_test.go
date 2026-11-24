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

package container

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
)

func skipIfNotAvailable(tb testing.TB, files ...string) {
	tb.Helper()
	for _, f := range files {
		if _, err := exec.LookPath(f); err != nil {
			tb.Skipf("%v is not available: %v", f, err)
		}
	}
}

func openAppendTestFile(tb testing.TB, name string) (string, *os.File) {
	tb.Helper()
	filename := path.Join(tb.TempDir(), name)
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		tb.Fatalf("open %s: %v", filename, err)
	}
	return filename, file
}

func readFileForLog(tb testing.TB, filename string) string {
	tb.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		tb.Logf("reading %s: %v", filename, err)
		return ""
	}
	return string(data)
}

func createImageEROFS(image, source string, options ...string) error {
	mkfs, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		return fmt.Errorf("mkfs.erofs is not available: %w", err)
	}
	cmd := fmt.Sprintf("%s %s %s %s", mkfs, strings.Join(options, " "), image, source)
	if out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput(); err != nil {
		return fmt.Errorf("exec: sh -c %q, err: %w, out: %s", cmd, err, out)
	}
	return nil
}
