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
	"path/filepath"

	"gvisor.dev/gvisor/pkg/urpc"
)

// ContMgrStartForked finishes initialization and resumes a forked sandbox.
const ContMgrStartForked = "containerManager.StartForked"

// TemplateServerAddr returns the filesystem path for a template listener.
func TemplateServerAddr(rootDir, id string) string {
	return filepath.Join(rootDir, fmt.Sprintf("runsc-%s.template.sock", id))
}

// BackendFileInfo describes a per-child backend file donated to a forked
// sandbox. MountPoint identifies the file-backed tmpfs/overlay mount that must
// switch to FilePayload.Files[FilestoreIndex].
type BackendFileInfo struct {
	MountPoint     string
	FilestoreIndex int
}

// TemplateForkArgs carries the new child sandbox's identity and the
// donated file descriptors:
//
//   - UserStdoutFileIndex, UserStderrFileIndex: append-only regular files for
//     the child container's stdout/stderr targets. They are dup2'd over the
//     template's own stdout/stderr in the post-clone child.
//   - UrpcServerFileIndex: a freshly created Unix socket for the child's
//     control plane. The child will register its own containerManager on this
//     socket and accept StartForked to finish initialization.
//   - DeviceFileIndex: a freshly opened platform device fd. Ownership
//     transfers to Platform.Reload.
//   - BackendFileInfo: per-child backend files for file-backed tmpfs/overlay
//     mounts.
type TemplateForkArgs struct {
	// ID is the new child sandbox's container id.
	ID string

	// Env overrides /proc/gvisor/spec_environ when non-nil.
	Env []string

	UserStdoutFileIndex int
	UserStderrFileIndex int
	UrpcServerFileIndex int
	DeviceFileIndex     int

	BackendFileInfo []BackendFileInfo

	urpc.FilePayload
}

// TemplateForkResult is the SandboxTemplate.Fork reply.
type TemplateForkResult struct{}
