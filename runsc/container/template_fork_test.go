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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/control/client"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/erofs"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

func TestTemplateForkExec(t *testing.T) {
	spec := lifecycleSpec()
	spec.Process.User.UID = 1000
	spec.Process.User.GID = 1001
	spec.Process.Cwd = "/data"
	spec.Process.NoNewPrivileges = true
	spec.Process.Capabilities = nil
	tmpl, conf := createLifecycleTemplate(t, 1, 1, spec)
	args := lifecycleForkArgs(t)
	args.BundleDir = tmpl.container.BundleDir
	args.Env = []string{"FORK_ENV=child"}
	child := lifecycleFork(t, tmpl, conf, args)
	flags := append(conf.ToFlags(), "exec", child.ID, "/busybox", "sh", "-c",
		`/busybox id -u; /busybox id -g; pwd; echo "$FORK_ENV"; /busybox grep '^NoNewPrivs:' /proc/self/status`)
	cmd := exec.Command(specutils.ExePath, flags...)
	cmd.Stderr = os.Stderr
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("runsc exec: %v", err)
	}
	if want := "1000\n1001\n/data\ntemplate\nNoNewPrivs:\t1\n"; string(got) != want {
		t.Errorf("runsc exec defaults: got %q, want %q", got, want)
	}
}

func TestTemplateForkLifecycle(t *testing.T) {
	tmpl, conf := newLifecycleTemplate(t, 1)
	first := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
	second := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
	conn, err := connectSandboxTemplateSocket(second.Sandbox.ControlSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Call(boot.ContMgrStartForked, &boot.CreateLinksAndRoutesArgs{}, nil); err == nil {
		t.Fatal("second StartForked succeeded")
	}
	if err := unix.Kill(first.Sandbox.Getpid(), unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	usage, err := second.Sandbox.Usage(true)
	if err != nil || usage.Anonymous == 0 {
		t.Fatalf("anonymous usage was not accounted: %+v, %v", usage, err)
	}
	if _, err := second.Sandbox.UsageFD(); err == nil {
		t.Fatal("UsageFD accepted anonymous memory without a shared backing file")
	}
	pidfd, err := unix.PidfdOpen(second.Sandbox.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	if err := DeleteTemplate(tmpl); err != nil {
		t.Fatal(err)
	}
	waitForPidfdExit(t, pidfd)
}

func waitForPidfdExit(t *testing.T, fd int) {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			t.Fatalf("timed out waiting for pidfd %d", fd)
		}
		n, err := unix.Poll(fds, int(timeout.Milliseconds())+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n != 1 || fds[0].Revents&unix.POLLIN == 0 {
			t.Fatalf("waiting for pidfd %d: poll=%d revents=%#x err=%v", fd, n, fds[0].Revents, err)
		}
		return
	}
}

// TestTemplateForkRuntime exercises scheduler recovery with concurrent children
// and repeated Sentry GC, including children with multiple scheduler Ps.
func TestTemplateForkRuntime(t *testing.T) {
	for _, cpuNum := range []int{1, 4} {
		t.Run(fmt.Sprintf("cpus_%d", cpuNum), func(t *testing.T) {
			tmpl, conf := newLifecycleTemplate(t, cpuNum)
			for child := range 8 {
				t.Run(fmt.Sprintf("child_%d", child), func(t *testing.T) {
					t.Parallel()
					c := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
					conn, err := connectSandboxTemplateSocket(c.Sandbox.ControlSocketPath)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					for range 20 {
						if err := conn.Call("Usage.Reduce", &control.UsageReduceOpts{Wait: true}, nil); err != nil {
							t.Fatalf("GC in forked Sentry: %v", err)
						}
					}
					if _, err := c.Sandbox.Usage(true); err != nil {
						t.Fatalf("child stopped responding after GC: %v", err)
					}
				})
			}
		})
	}
}

func TestTemplateForkFailureRecovery(t *testing.T) {
	tmpl, conf := newLifecycleTemplate(t, 1)
	args := lifecycleForkArgs(t)
	child := lifecycleFork(t, tmpl, conf, args)
	if duplicate, err := tmpl.Fork(conf, args); err == nil {
		duplicate.Destroy()
		t.Fatal("duplicate container ID accepted")
	}
	for _, mode := range []string{"device", "network", "cgroup", "environ"} {
		t.Run(mode, func(t *testing.T) {
			badConf := *conf
			badArgs := lifecycleForkArgs(t)
			switch mode {
			case "environ":
				badArgs.Env = []string{"INVALID"}
			case "device":
				badConf.PlatformDevicePath = "/dev/null"
			case "cgroup":
				badArgs.CgroupPath = "/sys/fs/cgroup/" + testutil.RandomContainerID()
			case "network":
				badArgs.NetworkArgs.FDBasedLinks = []boot.FDBasedLink{{Name: "missing-fd", NumChannels: 1}}
			}
			if c, err := tmpl.Fork(&badConf, badArgs); err == nil {
				c.Destroy()
				t.Fatal("invalid initialization succeeded")
			}
		})
	}
	for range 5 {
		conn, err := connectSandboxTemplateSocket(tmpl.seed.TemplateSocketPath(conf.RootDir))
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		conn, err = connectSandboxTemplateSocket(tmpl.seed.TemplateSocketPath(conf.RootDir))
		if err != nil {
			t.Fatal(err)
		}
		err = conn.Call("SandboxTemplate.Fork", &boot.TemplateForkArgs{ID: "missing-fds"}, nil)
		conn.Close()
		if err == nil {
			t.Fatal("malformed FD payload accepted")
		}
	}
	if _, err := child.Sandbox.Usage(true); err != nil {
		t.Fatalf("existing child affected by failed forks: %v", err)
	}
	lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
}

// TestTemplateForkState validates inherited anonymous and shared-file mappings
// against writes from concurrently running siblings.
func TestTemplateForkState(t *testing.T) {
	for _, overlay := range []string{"memory", "self"} {
		t.Run(overlay, func(t *testing.T) {
			root, image, cleanup := setupEROFSBusybox(t)
			t.Cleanup(cleanup)
			binary, err := testutil.FindFile("test/cmd/fork_state/fork_state")
			if err != nil {
				t.Fatal(err)
			}
			if err := testutil.Copy(binary, filepath.Join(root, "state")); err != nil {
				t.Fatal(err)
			}
			if err := createImageEROFS(image, root, "-E noinline_data"); err != nil {
				t.Fatal(err)
			}
			spec := testutil.NewSpecWithArgs("/state")
			spec.Root.Path = root
			spec.Root.Readonly = false
			spec.Mounts = nil
			spec.Annotations = map[string]string{
				boot.RootfsPrefix + "type":    erofs.Name,
				boot.RootfsPrefix + "source":  image,
				boot.RootfsPrefix + "overlay": overlay,
			}
			conf := templateForkConf(t)
			tmpl, _ := startTestTemplate(t, conf, spec, templateTestWorkers(t))
			if idle := os.Getenv("GVISOR_TEMPLATE_FORK_IDLE"); idle != "" {
				d, err := time.ParseDuration(idle)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(d)
			}
			var tokens [20]string
			t.Run("siblings", func(t *testing.T) {
				for i := range tokens {
					t.Run(strconv.Itoa(i), func(t *testing.T) {
						t.Parallel()
						args := lifecycleForkArgs(t)
						start := time.Now().UnixNano()
						child, err := tmpl.Fork(conf, args)
						if err != nil {
							t.Fatal(err)
						}
						defer child.Destroy()
						if status, err := child.Wait(); err != nil || status != 0 {
							t.Fatalf("state workload: status=%v err=%v stderr=%s", status, err, readFileForLog(t, args.Stderr.Name()))
						}
						fields := strings.Fields(readFileForLog(t, args.Stdout.Name()))
						if len(fields) != 2 || len(fields[0]) != 32 {
							t.Fatalf("unexpected state workload output: %q", fields)
						}
						tokens[i] = fields[0]
						end := time.Now().UnixNano()
						if now, err := strconv.ParseInt(fields[1], 10, 64); err != nil || now < start || now > end {
							t.Errorf("guest CLOCK_REALTIME = %q, want within [%d, %d]", fields[1], start, end)
						}
					})
				}
			})
			slices.Sort(tokens[:])
			if len(slices.Compact(tokens[:])) != len(tokens) {
				t.Error("siblings returned duplicate random tokens")
			}
		})
	}
}

func TestTemplateForkCgroup(t *testing.T) {
	tmpl, conf := newLifecycleTemplate(t, 1)
	inherited := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
	readCgroup := func(pid int) string {
		t.Helper()
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got, want := readCgroup(inherited.Sandbox.Getpid()), readCgroup(tmpl.PID); got != want {
		t.Fatalf("inherited cgroup = %q, want %q", got, want)
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("requires cgroup v2: %v", err)
	}
	dir, err := os.MkdirTemp("/sys/fs/cgroup", "gvisor-fork-test-")
	if err != nil {
		t.Skipf("requires a writable cgroup mount: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Errorf("removing cgroup: %v", err)
		}
	})
	childConf := *conf
	childConf.IgnoreCgroups = false
	args := lifecycleForkArgs(t)
	args.CgroupPath = "/" + filepath.Base(dir)
	child := lifecycleFork(t, tmpl, &childConf, args)
	if got, want := readCgroup(child.Sandbox.Getpid()), "0::"+args.CgroupPath+"\n"; got != want {
		t.Fatalf("child cgroup = %q, want %q", got, want)
	}
	if got := readCgroup(tmpl.PID); got == readCgroup(child.Sandbox.Getpid()) {
		t.Fatalf("moving the child also moved the template to %q", got)
	}
}

func TestTemplateForkCheckpointResume(t *testing.T) {
	tmpl, conf := newLifecycleTemplate(t, 1)
	for _, compression := range []statefile.CompressionLevel{
		statefile.CompressionLevelNone,
		statefile.CompressionLevelFlateBestSpeed,
	} {
		t.Run(string(compression), func(t *testing.T) {
			child := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
			if err := child.Checkpoint(conf, t.TempDir(), sandbox.CheckpointOpts{
				Compression: compression,
				Resume:      true,
			}); err != nil {
				t.Fatalf("checkpoint forked sandbox: %v", err)
			}
			if status, err := execute(conf, child, "/busybox", "true"); err != nil || status != 0 {
				t.Fatalf("guest execution after checkpoint: status=%v err=%v", status, err)
			}
		})
	}
}

// TestTemplateForkProcessIsolation checks the host process after guest startup.
// The production configuration requires runsc and its sidecars to be executable
// by nobody, including every directory in their paths.
func TestTemplateForkProcessIsolation(t *testing.T) {
	if os.Getenv("GVISOR_TEMPLATE_FORK_PRODUCTION") != "1" {
		t.Skip("set GVISOR_TEMPLATE_FORK_PRODUCTION=1 to test the production chroot")
	}
	tmpl, conf := newLifecycleTemplate(t, 4)
	for child := range 8 {
		t.Run(fmt.Sprintf("child_%d", child), func(t *testing.T) {
			t.Parallel()
			c := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
			proc := fmt.Sprintf("/proc/%d", c.Sandbox.Getpid())
			tasks, err := os.ReadDir(filepath.Join(proc, "task"))
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, task := range tasks {
				status, err := os.ReadFile(filepath.Join(proc, "task", task.Name(), "status"))
				if os.IsNotExist(err) {
					continue // The runtime may retire an idle thread.
				}
				if err != nil {
					t.Fatal(err)
				}
				fields := make(map[string][]string)
				for _, line := range strings.Split(string(status), "\n") {
					key, value, ok := strings.Cut(line, ":")
					if ok {
						fields[key] = strings.Fields(value)
					}
				}
				for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
					if got := strings.Join(fields[key], " "); got != "0000000000000000" {
						t.Errorf("thread %s %s = %q, want zero", task.Name(), key, got)
					}
				}
				for _, key := range []string{"Uid", "Gid"} {
					if got := strings.Join(fields[key], " "); got != "65534 65534 65534 65534" {
						t.Errorf("thread %s %s = %q, want nobody", task.Name(), key, got)
					}
				}
				for key, want := range map[string]string{"NoNewPrivs": "1", "Seccomp": "2"} {
					if got := strings.Join(fields[key], " "); got != want {
						t.Errorf("thread %s %s = %q, want %s", task.Name(), key, got, want)
					}
				}
				checked++
			}
			if checked == 0 {
				t.Fatal("no Sentry threads checked")
			}
			mounts, err := os.ReadFile(filepath.Join(proc, "mountinfo"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(mounts), " - proc ") {
				t.Errorf("child retained procfs mount:\n%s", mounts)
			}
		})
	}
}

// TestTemplateForkFileDescriptors checks that donated descriptors are not also
// inherited at their original numbers and retained across template forks.
func TestTemplateForkFileDescriptors(t *testing.T) {
	tmpl, conf := newLifecycleTemplate(t, 1)
	readFDs := func(pid int, template bool) map[string]bool {
		t.Helper()
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		sockets := make(map[string]bool)
		devices := 0
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(dir, entry.Name()))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if target == "/dev/kvm" || target == "/dev/slimvm" {
				devices++
			}
			if template && strings.Contains(target, "kvm-vm") {
				t.Errorf("template retained VM descriptor %s: %s", entry.Name(), target)
			}
			if strings.HasPrefix(target, "socket:") {
				if sockets[target] {
					t.Errorf("process %d retained duplicate socket %s", pid, target)
				}
				sockets[target] = true
			}
		}
		wantDevices := 0
		if !template && conf.Platform == "slimvm" {
			wantDevices = 1
		}
		if devices != wantDevices {
			t.Errorf("process %d has %d platform device descriptors, want %d", pid, devices, wantDevices)
		}
		return sockets
	}
	child := lifecycleFork(t, tmpl, conf, lifecycleForkArgs(t))
	templateSockets := readFDs(workerParentPID(t, child.SandboxPid()), true)
	for socket := range readFDs(child.Sandbox.Getpid(), false) {
		if templateSockets[socket] {
			t.Errorf("child retained template socket %s", socket)
		}
	}
}

func TestTemplateForkReusedID(t *testing.T) {
	seed, conf := newLifecycleTemplateWorkers(t, 1, 1)
	loaded, err := LoadTemplate(conf.RootDir, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Destroy(); err != nil {
		t.Fatal(err)
	}
	args := lifecycleForkArgs(t)
	replacement, err := CreateTemplate(conf, TemplateArgs{
		ID: seed.ID, Spec: seed.container.Spec, BundleDir: seed.container.BundleDir,
		Stdout: args.Stdout, Stderr: args.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := replacement.Destroy(); err != nil {
			t.Error(err)
		}
	})
	if c, err := seed.Fork(conf, lifecycleForkArgs(t)); err == nil {
		c.Destroy()
		t.Fatal("old template handle forked from replacement")
	}
	if err := seed.Destroy(); err != nil {
		t.Fatal(err)
	}
	lifecycleFork(t, replacement, conf, lifecycleForkArgs(t))
}

func TestTemplateForkHandle(t *testing.T) {
	template, conf := newLifecycleTemplateWorkers(t, 1, 4)
	if err := template.container.Saver.lock(); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			template.container.Saver.UnlockOrDie()
		}
	}()
	args := lifecycleForkArgs(t)
	done := make(chan error, 1)
	go func() {
		child, err := template.Fork(conf, args)
		if err == nil {
			err = child.Destroy()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		template.container.Saver.UnlockOrDie()
		locked = false
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		t.Fatal("fork blocked while template metadata was locked")
	}
}

func workerParentPID(t *testing.T, childPID int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", childPID))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "PPid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || pid <= 1 {
				t.Fatalf("invalid worker PID %q for child %d", value, childPID)
			}
			return pid
		}
	}
	t.Fatalf("missing PPid for child %d", childPID)
	return 0
}

func TestTemplateForkWorkers(t *testing.T) {
	const workers = 2
	template, conf := newLifecycleTemplateWorkers(t, 1, workers)
	children := func(pid int) []int {
		t.Helper()
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
		if err != nil {
			t.Fatal(err)
		}
		var result []int
		for _, word := range strings.Fields(string(data)) {
			id, err := strconv.Atoi(word)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, id)
		}
		return result
	}
	seeds := children(template.PID)
	if len(seeds) != 1 {
		t.Fatalf("template stub children=%v, want one seed", seeds)
	}
	pool := children(seeds[0])
	if len(pool) != workers {
		t.Fatalf("seed children=%v, want %d template workers", pool, workers)
	}
	first := lifecycleFork(t, template, conf, lifecycleForkArgs(t))
	worker := workerParentPID(t, first.SandboxPid())
	if !slices.Contains(pool, worker) {
		t.Fatalf("first child parent = %d, want a worker in %v", worker, pool)
	}
	// Pause the first worker so serving the next fork requires another worker,
	// regardless of the kernel's accept queue scheduling.
	proc, err := os.FindProcess(worker)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Release()
	if err := proc.Signal(unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer proc.Signal(unix.SIGCONT)
	if err := testutil.Poll(func() error {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", worker))
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "State:\tT") {
			return fmt.Errorf("worker %d has not stopped", worker)
		}
		return nil
	}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// Let the request finish even if only the first worker can accept.
	timer := time.AfterFunc(30*time.Second, func() { proc.Signal(unix.SIGCONT) })
	defer timer.Stop()
	second := lifecycleFork(t, template, conf, lifecycleForkArgs(t))
	if pid := workerParentPID(t, second.SandboxPid()); pid == worker || !slices.Contains(pool, pid) {
		t.Fatalf("second child parent = %d, want the other worker in %v", pid, pool)
	}
	if err := template.Destroy(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range append(seeds, pool...) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
			if os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("template process %d survived seed destruction", pid)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// Losing a worker must tear down the pool and its running instances.
func TestTemplateForkWorkerExit(t *testing.T) {
	template, conf := newLifecycleTemplateWorkers(t, 1, 4)
	child := lifecycleFork(t, template, conf, lifecycleForkArgs(t))
	worker := workerParentPID(t, child.SandboxPid())
	var pidfds []int
	for _, pid := range []int{template.PID, child.SandboxPid()} {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		pidfds = append(pidfds, fd)
	}
	if err := unix.Kill(worker, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for _, fd := range pidfds {
		waitForPidfdExit(t, fd)
	}
	if c, err := template.Fork(conf, lifecycleForkArgs(t)); err == nil {
		c.Destroy()
		t.Fatal("fork succeeded after the template pool exited")
	}
}

// TestTemplateForkEnviron verifies that children receive independent spec
// environments while the application's inherited environment is unchanged.
func TestTemplateForkEnviron(t *testing.T) {
	tmpl, conf := newLifecycleTemplateWorkers(t, 1, 4)
	loaded, err := LoadTemplate(conf.RootDir, tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"inherit", nil},
		{"empty", []string{}},
		{"first", []string{"FORK_ENV=first", "EMPTY=", "VALUE=a=b c\nnext"}},
		{"second", []string{"FORK_ENV=second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := lifecycleForkArgs(t)
			args.Env = tc.env
			child := lifecycleFork(t, loaded, conf, args)
			waitForFileLine(t, args.Stdout.Name(), "APP_ENV=template", 10*time.Second)
			got, err := executeCombinedOutput(conf, child, nil, "/busybox", "cat", "/proc/gvisor/spec_environ")
			if err != nil {
				t.Fatalf("reading spec_environ: %v", err)
			}
			if tc.env == nil {
				if !strings.Contains("\x00"+string(got), "\x00FORK_ENV=template\x00") {
					t.Fatalf("inherited spec_environ = %q, want FORK_ENV=template", got)
				}
			} else {
				want := ""
				if len(tc.env) != 0 {
					want = strings.Join(tc.env, "\x00") + "\x00"
				}
				if string(got) != want {
					t.Fatalf("spec_environ = %q, want %q", got, want)
				}
			}

		})
	}
}

func TestTemplateForkWritableRootfs(t *testing.T) {
	rootfsDir, rootfsImage, cleanup := setupEROFSBusybox(t)
	t.Cleanup(cleanup)

	spec := testutil.NewSpecWithArgs(
		"/busybox", "sh", "-c",
		"/busybox mkdir /fork-bin && "+
			"/busybox cp /busybox /fork-bin/busybox && "+
			"/busybox echo template > /fork-write && "+
			"exec 3<>/proc/gvisor/checkpoint; echo 1 >&3; /busybox cat <&3 | /busybox grep -qx restore && "+
			"/fork-bin/busybox cat /fork-write && "+
			"/fork-bin/busybox echo child >> /fork-write && "+
			"/fork-bin/busybox cat /fork-write && "+
			"/fork-bin/busybox echo OK",
	)
	spec.Root.Path = rootfsDir
	spec.Root.Readonly = false
	spec.Mounts = nil
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations[boot.RootfsPrefix+"type"] = erofs.Name
	spec.Annotations[boot.RootfsPrefix+"source"] = rootfsImage
	spec.Annotations[boot.RootfsPrefix+"overlay"] = config.SelfOverlay.String()

	conf := templateForkConf(t)
	template, _ := startTestTemplate(t, conf, spec, templateTestWorkers(t))
	templateFilestorePath := boot.SelfFilestorePath(rootfsDir, template.ID)
	if _, err := os.Stat(templateFilestorePath); err != nil {
		t.Fatalf("stat template filestore %q: %v", templateFilestorePath, err)
	}

	args := lifecycleForkArgs(t)
	child, err := template.Fork(conf, args)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := child.Destroy(); err != nil {
			t.Error(err)
		}
	})
	if status, err := child.Wait(); err != nil || status != 0 {
		t.Fatalf("child exited with %v, %v: %s", status, err, readFileForLog(t, args.Stderr.Name()))
	}
	if got := readFileForLog(t, args.Stdout.Name()); got != "template\ntemplate\nchild\nOK\n" {
		t.Fatalf("unexpected overlay contents: %q", got)
	}

	childFilestorePath := boot.SelfFilestorePath(rootfsDir, child.ID)
	if _, err := os.Stat(childFilestorePath); err != nil {
		t.Fatalf("stat child filestore %q: %v", childFilestorePath, err)
	}
	if err := child.Destroy(); err != nil {
		t.Fatalf("child.Destroy: %v", err)
	}
	if _, err := os.Stat(childFilestorePath); !os.IsNotExist(err) {
		t.Fatalf("stat child filestore %q after Destroy = %v, want not exist", childFilestorePath, err)
	}
	if err := DeleteTemplate(template); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := os.Stat(templateFilestorePath); !os.IsNotExist(err) {
		t.Fatalf("stat template filestore %q after DeleteTemplate = %v, want not exist", templateFilestorePath, err)
	}
}

func TestTemplateConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name: "seccomp",
			mutate: func(conf *config.Config) {
				conf.DisableSeccomp = true
			},
			wantErr: "requires Sentry seccomp",
		},
		{
			name: "platform",
			mutate: func(conf *config.Config) {
				conf.Platform = "ptrace"
			},
			wantErr: "requires --platform=kvm or --platform=slimvm",
		},
		{
			name: "network",
			mutate: func(conf *config.Config) {
				conf.Network = config.NetworkHost
			},
			wantErr: "requires --network=sandbox or --network=none",
		},
		{
			name: "directfs",
			mutate: func(conf *config.Config) {
				conf.DirectFS = true
			},
			wantErr: "incompatible with --directfs",
		},
		{
			name: "rootless",
			mutate: func(conf *config.Config) {
				conf.Rootless = true
			},
			wantErr: "incompatible with --rootless",
		},
		{
			name: "metric-server",
			mutate: func(conf *config.Config) {
				conf.MetricServer = "localhost:1234"
			},
			wantErr: "does not support metric server registration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := templateValidationConfig(t)
			tc.mutate(conf)
			err := validateTemplateForkConfig(conf, "sandbox template")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTemplateForkConfig() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCreateTemplateRejectsTerminal(t *testing.T) {
	spec := templateValidationSpec()
	spec.Process.Terminal = true
	_, err := CreateTemplate(templateValidationConfig(t), TemplateArgs{
		Workers: templateTestWorkers(t),
		ID:      "template",
		Spec:    spec,
	})
	if err == nil || !strings.Contains(err.Error(), "do not support terminal stdio") {
		t.Fatalf("CreateTemplate() = %v, want terminal stdio error", err)
	}
}

func TestCreateTemplateRejectsNonRegularStdio(t *testing.T) {
	stdin, stdout, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer stdin.Close()
	defer stdout.Close()
	_, err = CreateTemplate(templateValidationConfig(t), TemplateArgs{
		Workers: templateTestWorkers(t),
		ID:      "template",
		Spec:    templateValidationSpec(),
		Stdout:  stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "template stdout must be a regular file") {
		t.Fatalf("CreateTemplate() = %v, want regular stdout error", err)
	}
}

func TestTemplateStateLifecycleView(t *testing.T) {
	rootDir := t.TempDir()
	writeContainerStateForTemplateTest(t, rootDir, "regular", false, nil)
	state := &TemplateState{
		Ready:    true,
		Platform: "kvm",
		BackendFiles: []TemplateBackendFile{{
			MountPoint: "/",
			MountSrc:   "/rootfs",
			GoferConf:  "lisafs:self",
		}},
	}
	writeContainerStateForTemplateTest(t, rootDir, "template", true, state)

	if _, err := Load(rootDir, FullID{SandboxID: "template", ContainerID: "template"}, LoadOpts{
		Exact:     true,
		SkipCheck: true,
	}); err == nil || !strings.Contains(err.Error(), "is a sandbox template") {
		t.Fatalf("generic Load(template) = %v, want template rejection", err)
	}

	template, err := LoadTemplate(rootDir, "template")
	if err != nil {
		t.Fatalf("LoadTemplate: %v", err)
	}
	if !template.Ready || template.Platform != "kvm" {
		t.Errorf("LoadTemplate() = %+v, want ready KVM template", template)
	}
	if got := template.container.TemplateState.BackendFiles; len(got) != 1 || got[0].MountPoint != "/" {
		t.Errorf("LoadTemplate backend files = %+v, want root backend", got)
	}
	if err := adjustSandboxOOMScoreAdj(template.container.Sandbox, template.container.Spec, rootDir, false); err != nil {
		t.Errorf("adjustSandboxOOMScoreAdj(template): %v", err)
	}

	containers, err := List(rootDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(containers) != 1 || containers[0].ContainerID != "regular" {
		t.Errorf("List() = %+v, want only regular container", containers)
	}

	templates, err := ListTemplates(rootDir)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(templates) != 1 || templates[0].ID != "template" {
		t.Errorf("ListTemplates() = %+v, want only template", templates)
	}
}

func TestForkRejectsTemplatePlatformMismatch(t *testing.T) {
	conf := templateValidationConfig(t)
	conf.Platform = "slimvm"
	writeContainerStateForTemplateTest(t, conf.RootDir, "template", true, &TemplateState{
		Ready:    true,
		Platform: "kvm",
	})

	template, err := LoadTemplate(conf.RootDir, "template")
	if err != nil {
		t.Fatal(err)
	}
	_, err = template.Fork(conf, ForkArgs{
		ID:          "child",
		NetworkArgs: &boot.CreateLinksAndRoutesArgs{},
	})
	if err == nil || !strings.Contains(err.Error(), `uses platform "kvm", but fork requested "slimvm"`) {
		t.Fatalf("template.Fork() = %v, want platform mismatch", err)
	}
}

func TestCreateTemplateRejectsGoferBackedMount(t *testing.T) {
	rootfsDir, rootfsImage, cleanup := setupEROFSBusybox(t)
	defer cleanup()

	spec := testutil.NewSpecWithArgs(
		"/busybox", "sh", "-c",
		"exec 3<>/proc/gvisor/checkpoint; echo 1 >&3; /busybox cat <&3 | /busybox grep -qx restore",
	)
	spec.Root.Path = rootfsDir
	spec.Mounts = []specs.Mount{{
		Destination: "/tmp",
		Type:        "bind",
		Source:      t.TempDir(),
		Options:     []string{"rbind", "rw"},
	}}
	spec.Annotations = map[string]string{
		boot.RootfsPrefix + "type":    erofs.Name,
		boot.RootfsPrefix + "source":  rootfsImage,
		boot.RootfsPrefix + "overlay": config.NoOverlay.String(),
	}

	conf := templateForkConf(t)
	_, bundleDir, bundleCleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("SetupContainer: %v", err)
	}
	defer bundleCleanup()

	_, stdout := openAppendTestFile(t, "template-stdout")
	_, stderr := openAppendTestFile(t, "template-stderr")
	defer stdout.Close()
	defer stderr.Close()

	templateID := testutil.RandomContainerID()
	_, err = CreateTemplate(conf, TemplateArgs{
		Workers:   templateTestWorkers(t),
		ID:        templateID,
		Spec:      spec,
		BundleDir: bundleDir,
		Stdout:    stdout,
		Stderr:    stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "must be goferless") {
		t.Fatalf("CreateTemplate() = %v, want goferless error", err)
	}
	if _, err := LoadTemplate(conf.RootDir, templateID); err == nil {
		t.Fatalf("LoadTemplate(%q) succeeded after failed creation", templateID)
	}
}

func TestTemplateForkPreservesExistingFilestore(t *testing.T) {
	mountSrc := t.TempDir()
	childID := "fork-child"
	infos := []TemplateBackendFile{{
		MountPoint: "/",
		MountSrc:   mountSrc,
		GoferConf: specutils.GoferMountConf{
			Lower: specutils.Erofs,
			Upper: specutils.SelfOverlay,
		}.String(),
	}}

	path := boot.SelfFilestorePath(mountSrc, childID)
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	files, _, err := createForkOverlayBackendFiles(infos, childID)
	defer closeForkBackendFiles(files)
	if err == nil {
		t.Fatal("fork reused an existing filestore")
	}
	if got := readFileForLog(t, path); got != "existing" {
		t.Fatalf("filestore contents after failed fork = %q, want existing", got)
	}
}

func templateValidationConfig(t *testing.T) *config.Config {
	t.Helper()
	conf := testutil.TestConfig(t)
	conf.RootDir = t.TempDir()
	conf.Platform = templateForkPlatform(t)
	conf.Network = config.NetworkSandbox
	conf.DirectFS = false
	conf.Rootless = false
	conf.MetricServer = ""
	return conf
}

func templateValidationSpec() *specs.Spec {
	spec := testutil.NewSpecWithArgs("/bin/true")
	spec.Mounts = nil
	spec.Annotations = map[string]string{
		boot.RootfsPrefix + "type":   erofs.Name,
		boot.RootfsPrefix + "source": "/rootfs.img",
	}
	return spec
}

func writeContainerStateForTemplateTest(t *testing.T, rootDir, id string, isTemplate bool, templateState *TemplateState) {
	t.Helper()
	sand := &sandbox.Sandbox{
		ID:       id,
		UID:      -1,
		GID:      -1,
		Template: isTemplate,
	}
	sand.SetRootDir(rootDir)
	c := &Container{
		allowWrite:    true,
		ID:            id,
		Spec:          testutil.NewSpecWithArgs("/bin/true"),
		Status:        Stopped,
		Sandbox:       sand,
		TemplateState: templateState,
		Saver: StateFile{
			RootDir: rootDir,
			ID: FullID{
				SandboxID:   id,
				ContainerID: id,
			},
		},
	}
	if err := c.Saver.LockForNew(); err != nil {
		t.Fatalf("LockForNew(%q): %v", id, err)
	}
	if err := c.saveLocked(); err != nil {
		c.Saver.UnlockOrDie()
		t.Fatalf("saveLocked(%q): %v", id, err)
	}
	c.Saver.UnlockOrDie()
	if err := c.Saver.close(); err != nil {
		t.Fatalf("closing state file %q: %v", id, err)
	}
	t.Cleanup(func() {
		state := StateFile{
			RootDir: rootDir,
			ID: FullID{
				SandboxID:   id,
				ContainerID: id,
			},
		}
		_ = state.Destroy()
	})
}

func TestTemplateForkBackendFilesAnonOverlay(t *testing.T) {
	anonDir := t.TempDir()
	infos := []TemplateBackendFile{{
		MountPoint: "/work",
		MountSrc:   "/unused",
		GoferConf: specutils.GoferMountConf{
			Lower: specutils.Erofs,
			Upper: specutils.AnonOverlay,
		}.String(),
		AnonDir: anonDir,
	}}

	backendFiles, selfFilestorePaths, err := createForkOverlayBackendFiles(infos, "fork-child")
	if err != nil {
		t.Fatalf("createForkOverlayBackendFiles: %v", err)
	}
	defer closeForkBackendFiles(backendFiles)

	if got, want := len(backendFiles), 1; got != want {
		t.Fatalf("len(backendFiles) = %d, want %d", got, want)
	}
	if got, want := backendFiles[0].MountPoint, "/work"; got != want {
		t.Fatalf("backendFiles[0].MountPoint = %q, want %q", got, want)
	}
	if backendFiles[0].File == nil {
		t.Fatalf("backendFiles[0].File is nil")
	}
	if got := len(selfFilestorePaths); got != 0 {
		t.Fatalf("len(selfFilestorePaths) = %d, want 0", got)
	}
	if _, err := os.Stat(backendFiles[0].File.Name()); !os.IsNotExist(err) {
		t.Fatalf("stat anonymous filestore %q = %v, want not exist", backendFiles[0].File.Name(), err)
	}
	if _, err := backendFiles[0].File.Write([]byte("ok")); err != nil {
		t.Fatalf("write anonymous filestore: %v", err)
	}
}

func TestTemplateForkBackendFilesCleanupOnError(t *testing.T) {
	mountSrc := t.TempDir()
	childID := "fork-child"
	infos := []TemplateBackendFile{
		{
			MountPoint: "/",
			MountSrc:   mountSrc,
			GoferConf: specutils.GoferMountConf{
				Lower: specutils.Erofs,
				Upper: specutils.SelfOverlay,
			}.String(),
		},
		{
			MountPoint: "/work",
			MountSrc:   "/unused",
			GoferConf: specutils.GoferMountConf{
				Lower: specutils.Erofs,
				Upper: specutils.AnonOverlay,
			}.String(),
		},
	}

	backendFiles, selfFilestorePaths, err := createForkOverlayBackendFiles(infos, childID)
	if err == nil {
		closeForkBackendFiles(backendFiles)
		for _, path := range selfFilestorePaths {
			_ = os.Remove(path)
		}
		t.Fatalf("createForkOverlayBackendFiles unexpectedly succeeded")
	}
	path := boot.SelfFilestorePath(mountSrc, childID)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat self filestore %q after error = %v, want not exist", path, err)
	}
}

func TestCollectTemplateBackendFiles(t *testing.T) {
	rootfs := t.TempDir()
	anonDir := t.TempDir()
	conf := testutil.TestConfig(t)
	if err := conf.Overlay2.Set("all:" + config.AnonOverlayPrefix + anonDir); err != nil {
		t.Fatalf("Overlay2.Set: %v", err)
	}
	spec := testutil.NewSpecWithArgs("/bin/true")
	spec.Root.Path = rootfs
	spec.Root.Readonly = false
	spec.Mounts = nil
	c := &Container{
		Spec: spec,
		GoferMountConfs: specutils.GoferMountConfFlags{
			{
				Lower: specutils.Erofs,
				Upper: specutils.AnonOverlay,
			},
		},
	}
	rootfsHint := &boot.RootfsHint{
		Overlay: config.OverlayMedium(config.AnonOverlayPrefix + anonDir),
	}

	got, err := collectTemplateBackendFiles(conf, c, &boot.PodMountHints{}, rootfsHint)
	if err != nil {
		t.Fatalf("collectTemplateBackendFiles: %v", err)
	}
	if gotLen, wantLen := len(got), 1; gotLen != wantLen {
		t.Fatalf("len(backendFiles) = %d, want %d", gotLen, wantLen)
	}
	if got[0].MountPoint != "/" || got[0].MountSrc != rootfs || got[0].AnonDir != anonDir {
		t.Fatalf("backendFiles[0] = %+v, want root anon backend for %q in %q", got[0], rootfs, anonDir)
	}
}

func TestGoferlessFilestoresSelfOverlay(t *testing.T) {
	rootfs := t.TempDir()
	c := templateForkFilestoreTestContainer(rootfs, specutils.GoferMountConf{
		Lower: specutils.Erofs,
		Upper: specutils.SelfOverlay,
	})
	var ovl config.Overlay2
	if err := ovl.Set("root:self"); err != nil {
		t.Fatalf("Overlay2.Set: %v", err)
	}

	files, err := c.createGoferFilestoresGoferless(&config.Config{}, ovl, &boot.PodMountHints{})
	if err != nil {
		t.Fatalf("createGoferFilestoresGoferless: %v", err)
	}
	defer closeFiles(files)

	if got, want := len(files), 1; got != want {
		t.Fatalf("len(files) = %d, want %d", got, want)
	}
	wantPath := boot.SelfFilestorePath(rootfs, c.sandboxID())
	wantInfo, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat self filestore %q: %v", wantPath, err)
	}
	gotInfo, err := files[0].Stat()
	if err != nil {
		t.Fatalf("stat self filestore FD: %v", err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("filestore FD does not refer to %q", wantPath)
	}
	if err := os.Remove(wantPath); err != nil {
		t.Fatalf("remove self filestore %q: %v", wantPath, err)
	}
}

func TestGoferlessFilestoresAnonOverlay(t *testing.T) {
	rootfs := t.TempDir()
	anonDir := t.TempDir()
	c := templateForkFilestoreTestContainer(rootfs, specutils.GoferMountConf{
		Lower: specutils.Erofs,
		Upper: specutils.AnonOverlay,
	})
	var ovl config.Overlay2
	if err := ovl.Set("root:" + config.AnonOverlayPrefix + anonDir); err != nil {
		t.Fatalf("Overlay2.Set: %v", err)
	}

	files, err := c.createGoferFilestoresGoferless(&config.Config{}, ovl, &boot.PodMountHints{})
	if err != nil {
		t.Fatalf("createGoferFilestoresGoferless: %v", err)
	}
	defer closeFiles(files)

	if got, want := len(files), 1; got != want {
		t.Fatalf("len(files) = %d, want %d", got, want)
	}
	if _, err := os.Stat(files[0].Name()); !os.IsNotExist(err) {
		t.Fatalf("stat anonymous filestore %q = %v, want not exist", files[0].Name(), err)
	}
	if _, err := files[0].Write([]byte("ok")); err != nil {
		t.Fatalf("write anonymous filestore: %v", err)
	}
}

func templateForkFilestoreTestContainer(rootfs string, rootfsConf specutils.GoferMountConf) *Container {
	spec := testutil.NewSpecWithArgs("/bin/true")
	spec.Root.Path = rootfs
	spec.Root.Readonly = false
	spec.Mounts = nil
	return &Container{
		ID:   "container",
		Spec: spec,
		Saver: StateFile{
			ID: FullID{
				SandboxID: "sandbox",
			},
		},
		GoferMountConfs: specutils.GoferMountConfFlags{rootfsConf},
	}
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func waitForFileLine(tb testing.TB, filename, want string, timeout time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	var got string
	for {
		got = readFileForLog(tb, filename)
		for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if line == want {
				return
			}
		}
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %q in %s; got %q", want, filename, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setupEROFSBusybox creates a busybox EROFS rootfs and returns the
// rootfs dir, image path, and a cleanup function.
func setupEROFSBusybox(t *testing.T) (rootfsDir, rootfsImage string, cleanup func()) {
	t.Helper()
	skipIfNotAvailable(t, "mkfs.erofs", "busybox")
	if _, err := os.Stat(templateForkDevicePath(t)); err != nil {
		t.Skipf("requires selected platform device: %v", err)
	}
	testDir, err := os.MkdirTemp(testutil.TmpDir(), "template_fork_")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	rootfsDir, rootfsImage, err = createRootfsEROFS(testDir)
	if err != nil {
		t.Fatalf("createRootfsEROFS: %v", err)
	}
	return rootfsDir, rootfsImage, func() { os.RemoveAll(testDir) }
}

func templateForkConf(t *testing.T) *config.Config {
	t.Helper()
	conf := testutil.TestConfig(t)
	conf.Platform = templateForkPlatform(t)
	conf.Network = config.NetworkSandbox
	conf.DirectFS = false
	conf.CPUNum = 1
	if os.Getenv("GVISOR_TEMPLATE_FORK_PRODUCTION") == "1" {
		conf.TestOnlyAllowRunAsCurrentUserWithoutChroot = false
	}
	return conf
}

// templateForkPlatform selects the platform for the template integration tests.
func templateForkPlatform(t *testing.T) string {
	t.Helper()
	p := os.Getenv("GVISOR_TEMPLATE_FORK_E2E_PLATFORM")
	if p == "" {
		p = "kvm"
	}
	if p != "kvm" && p != "slimvm" {
		t.Fatalf("unsupported template platform %q", p)
	}
	return p
}

func templateForkDevicePath(t *testing.T) string {
	if templateForkPlatform(t) == "slimvm" {
		return "/dev/slimvm"
	}
	return "/dev/kvm"
}

func connectSandboxTemplateSocket(path string) (*urpc.Client, error) {
	if len(path) < linux.UnixPathMax {
		return client.ConnectTo(path)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening sandbox template socket %q: %w", path, err)
	}
	defer unix.Close(fd)
	return client.ConnectTo(filepath.Join("/proc/self/fd", strconv.Itoa(fd)))
}

func templateTestWorkers(t testing.TB) int {
	t.Helper()
	value := os.Getenv("GVISOR_TEMPLATE_FORK_WORKERS")
	if value == "" {
		return 1
	}
	workers, err := strconv.Atoi(value)
	if err != nil || workers < 1 {
		t.Fatalf("invalid template worker count %q", value)
	}
	return workers
}

func newLifecycleTemplate(t *testing.T, cpuNum int) (*Template, *config.Config) {
	return newLifecycleTemplateWorkers(t, cpuNum, templateTestWorkers(t))
}

func newLifecycleTemplateWorkers(t *testing.T, cpuNum, workers int) (*Template, *config.Config) {
	t.Helper()
	return createLifecycleTemplate(t, cpuNum, workers, lifecycleSpec())
}

func lifecycleSpec() *specs.Spec {
	spec := testutil.NewSpecWithArgs("/busybox", "sh", "-c",
		"/busybox echo TEMPLATE_INITIALIZED; exec 3<>/proc/gvisor/checkpoint; exec 4</proc/gvisor/checkpoint; echo 1 >&3; /busybox cat <&3 | /busybox grep -qx restore && /busybox cat <&4 | /busybox grep -qx restore && /busybox echo APP_ENV=$FORK_ENV && /busybox echo READY && /busybox sleep 3600")
	spec.Process.Env = append(spec.Process.Env, "FORK_ENV=template")
	return spec
}

func createLifecycleTemplate(t *testing.T, cpuNum, workers int, spec *specs.Spec) (*Template, *config.Config) {
	t.Helper()
	root, image, cleanup := setupEROFSBusybox(t)
	t.Cleanup(cleanup)
	spec.Root.Path = root
	spec.Mounts = nil
	spec.Annotations = map[string]string{
		boot.RootfsPrefix + "type":    erofs.Name,
		boot.RootfsPrefix + "source":  image,
		boot.RootfsPrefix + "overlay": config.NoOverlay.String(),
	}
	conf := templateForkConf(t)
	conf.CPUNum = cpuNum
	tmpl, stdout := startTestTemplate(t, conf, spec, workers)
	if got := readFileForLog(t, stdout); strings.Count(got, "TEMPLATE_INITIALIZED\n") != 1 {
		t.Fatalf("seed initialization output = %q, want exactly one initialization", got)
	}
	return tmpl, conf
}

func startTestTemplate(t *testing.T, conf *config.Config, spec *specs.Spec, workers int) (*Template, string) {
	t.Helper()
	_, bundle, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	outPath, stdout := openAppendTestFile(t, "template-stdout")
	_, stderr := openAppendTestFile(t, "template-stderr")
	defer stdout.Close()
	defer stderr.Close()
	tmpl, err := CreateTemplate(conf, TemplateArgs{
		ID: testutil.RandomContainerID(), Spec: spec, Workers: workers,
		BundleDir: bundle, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v; stderr: %s", err, readFileForLog(t, stderr.Name()))
	}
	t.Cleanup(func() {
		if err := tmpl.Destroy(); err != nil {
			t.Errorf("destroy template: %v", err)
		}
	})
	return tmpl, outPath
}

func lifecycleForkArgs(t *testing.T) ForkArgs {
	t.Helper()
	_, stdout := openAppendTestFile(t, "child-stdout")
	_, stderr := openAppendTestFile(t, "child-stderr")
	t.Cleanup(func() { stdout.Close(); stderr.Close() })
	return ForkArgs{
		ID:     testutil.RandomContainerID(),
		Stdout: stdout, Stderr: stderr, NetworkArgs: &boot.CreateLinksAndRoutesArgs{},
	}
}

func lifecycleFork(t *testing.T, tmpl *Template, conf *config.Config, args ForkArgs) *Container {
	t.Helper()
	child, err := tmpl.Fork(conf, args)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() {
		if err := child.Destroy(); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	})
	if pid := child.SandboxPid(); pid <= 0 || pid == os.Getpid() || pid == tmpl.PID {
		t.Fatalf("fork returned PID %d, caller PID %d, template PID %d", pid, os.Getpid(), tmpl.PID)
	}
	identity, err := specutils.ProcessIdentity(child.SandboxPid())
	if err != nil || child.Sandbox.PidIdentity != identity {
		t.Fatalf("forked process identity = %d, want %d: %v", child.Sandbox.PidIdentity, identity, err)
	}
	waitForFileLine(t, args.Stdout.Name(), "READY", 10*time.Second)
	return child
}
