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

// Minimal wait-template reaper stub. The template sandbox's runtime.Clone()
// execve's this binary on the parent branch so that the parent
// becomes a tiny C process with no Go runtime, no seccomp-triggering
// mmaps, and no signal handler conflicts. Its only job is to reap
// child processes via wait4.
#include <errno.h>
#include <sys/wait.h>

int main() {
  for (;;) {
    if (wait4(-1, 0, 0, 0) < 0 && errno != EINTR) break;
  }
}
