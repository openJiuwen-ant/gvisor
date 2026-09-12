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
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/config"
)

func TestStartForkedRequiresStartedTemplate(t *testing.T) {
	for _, l := range []*Loader{{state: started}, {templateSandbox: true, state: created}} {
		cm := containerManager{l: l}
		if err := cm.StartForked(&CreateLinksAndRoutesArgs{}, nil); err == nil {
			t.Error("StartForked accepted an invalid startup state")
		}
	}
}

func TestTemplateRejectsPauseResume(t *testing.T) {
	cm := containerManager{l: &Loader{templateSandbox: true}}
	if err := cm.Pause(nil, nil); err == nil {
		t.Error("Pause succeeded for a template")
	}
	if err := cm.Resume(nil, nil); err == nil {
		t.Error("Resume succeeded for a template")
	}
}

func TestForkWatchdogOpts(t *testing.T) {
	opts, err := forkWatchdogOpts(&config.Config{WatchdogAction: "log"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.StartupTimeoutAction != watchdog.Panic || opts.TaskTimeoutAction != watchdog.LogWarning {
		t.Errorf("watchdog actions: startup=%v, task=%v", opts.StartupTimeoutAction, opts.TaskTimeoutAction)
	}
}

func TestTemplateOutputsOutliveGuestFDTable(t *testing.T) {
	l, cleanup, err := createLoader(testConfig(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Destroy()
	defer cleanup()

	out, err := os.OpenFile(filepath.Join(t.TempDir(), "out"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stdio, err := fd.NewFromFiles([]*os.File{out, out, out})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, f := range stdio {
			f.Close()
		}
	}()
	outputFDs := [...]int{stdio[1].FD(), stdio[2].FD()}
	ctx := l.k.SupervisorContext()
	table, _, err := createFDTable(ctx, false, stdio, nil, specs.User{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	err = l.pinTemplateOutputs(table)
	table.DecRef(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var want unix.Stat_t
	if err := unix.Fstat(int(out.Fd()), &want); err != nil {
		t.Fatal(err)
	}
	for _, outputFD := range outputFDs {
		var got unix.Stat_t
		if err := unix.Fstat(outputFD, &got); err != nil {
			t.Fatalf("stat after guest closed stdio: %v", err)
		}
		if got.Dev != want.Dev || got.Ino != want.Ino {
			t.Errorf("output FD %d identifies a different file after guest closed stdio", outputFD)
		}
	}
}

func TestTemplateForkArgsValidation(t *testing.T) {
	stdio, err := os.OpenFile(filepath.Join(t.TempDir(), "stdio"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer stdio.Close()
	socketFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	socket := os.NewFile(uintptr(socketFD), "control")
	defer socket.Close()
	device, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	backend, err := os.CreateTemp(t.TempDir(), "backing")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	newArgs := func() *TemplateForkArgs {
		return &TemplateForkArgs{
			ID:                  "child",
			Env:                 []string{"KEY=value", "EMPTY=", "EQUALS=a=b"},
			UserStdoutFileIndex: 0,
			UserStderrFileIndex: 1,
			UrpcServerFileIndex: 2,
			DeviceFileIndex:     3,
			BackendFileInfo:     []BackendFileInfo{{MountPoint: "/", FilestoreIndex: 4}},
			FilePayload:         urpc.FilePayload{Files: []*os.File{stdio, stdio, socket, device, backend}},
		}
	}
	files, err := validateTemplateForkArgs(newArgs())
	if err != nil || len(files) != 1 || files["/"] != backend {
		t.Fatalf("backend donation = %v, %v; want the file for /", files, err)
	}

	for _, tc := range []struct {
		name   string
		change func(*TemplateForkArgs)
	}{
		{"invalid environment", func(a *TemplateForkArgs) { a.Env = []string{"KEY"} }},
		{"missing FD", func(a *TemplateForkArgs) { a.Files = a.Files[:4] }},
		{"nil FD", func(a *TemplateForkArgs) { a.Files[0] = nil }},
		{"invalid index", func(a *TemplateForkArgs) { a.DeviceFileIndex = -1 }},
		{"wrong control type", func(a *TemplateForkArgs) { a.Files[2] = stdio }},
		{"wrong device type", func(a *TemplateForkArgs) { a.Files[3] = stdio }},
		{"backend overlaps stdio", func(a *TemplateForkArgs) { a.BackendFileInfo[0].FilestoreIndex = 0 }},
		{"duplicate mount", func(a *TemplateForkArgs) {
			a.Files = append(a.Files, backend)
			a.BackendFileInfo = append(a.BackendFileInfo, BackendFileInfo{MountPoint: "/", FilestoreIndex: 5})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := newArgs()
			tc.change(args)
			if _, err := validateTemplateForkArgs(args); err == nil {
				t.Error("accepted invalid fork arguments")
			}
		})
	}
}

func TestStartForkedRequiresNetwork(t *testing.T) {
	cm := containerManager{l: &Loader{state: started, templateSandbox: true}}
	if err := cm.StartForked(nil, nil); err == nil {
		t.Error("StartForked accepted missing network arguments")
	}
}

// TestTemplateCheckpointRequest verifies that concurrent requests select one
// initialization barrier without turning later requests into completed saves.
func TestTemplateCheckpointRequest(t *testing.T) {
	l := Loader{templateSandbox: true, templateCheckpoint: make(chan struct{})}
	const callers = 20
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- l.SaveAsync() }()
	}
	accepted := 0
	for i := 0; i < callers; i++ {
		err := <-results
		if err == nil {
			accepted++
		} else if !linuxerr.Equals(linuxerr.EBUSY, err) {
			t.Errorf("SaveAsync: %v, want EBUSY", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d requests, want 1", accepted)
	}
	select {
	case <-l.templateCheckpoint:
	default:
		t.Fatal("checkpoint request did not notify the template")
	}
}
