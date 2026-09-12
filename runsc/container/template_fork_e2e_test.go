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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/control/client"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/erofs"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
	runscflag "gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/hostsettings"
	"gvisor.dev/gvisor/runsc/specutils"
)

const (
	templateForkE2ENetNSEnv             = "GVISOR_TEMPLATE_FORK_E2E_NETNS"
	templateForkE2EPlatformEnv          = "GVISOR_TEMPLATE_FORK_E2E_PLATFORM"
	templateForkE2EIterationsPerWorker  = 100
	templateForkE2EParallelReadyTimeout = 30 * time.Second
)

var (
	templateForkE2ENetworkSeq atomic.Uint32
	templateForkE2ECPUNum     = 1
)

// TestTemplateForkE2E exercises the external template/fork API shape. Normal
// start uses the runsc binary for create and the container API for start, while
// fork uses the container API for the complete operation. Both paths use
// explicit CreateLinksAndRoutes network setup, an EROFS rootfs, and a small
// static PID1 that signals readiness over TCP.
func TestTemplateForkE2E(t *testing.T) {
	skipIfNotAvailable(t, "mkfs.erofs")
	platform := templateForkE2EPlatform(t)
	if dev := templateForkE2EDevicePath(t, platform); dev != "" {
		if _, err := os.Stat(dev); err != nil {
			t.Fatalf("selected platform %q requires %s: %v", platform, dev, err)
		}
	}

	rootfsDir, rootfsImage := setupTemplateForkE2ERootfs(t)
	netPool := setupTemplateForkE2ENetworkPool(t, 1)

	conf := templateForkE2EConfForPlatform(t, platform)

	t.Run("normal_start", func(t *testing.T) {
		netEnv := netPool.networks[0]
		networkArgs, networkCleanup := netEnv.createNetworkArgs(t)
		defer networkCleanup()
		spec := newReadySpec(rootfsDir, rootfsImage, netPool.hostIP.String(), netPool.ready.port, false)
		_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
		if err != nil {
			t.Fatalf("SetupContainer: %v", err)
		}
		defer cleanup()

		stdoutPath, stdoutW := openAppendTestFile(t, "normal-stdout")
		stderrPath, stderrW := openAppendTestFile(t, "normal-stderr")
		defer stdoutW.Close()
		defer stderrW.Close()
		defer logStdioOnFailure(t, stdoutPath, stderrPath)

		cid := testutil.RandomContainerID()
		var c *Container
		start := time.Now()
		c, err = createWithRunscBinary(conf, cid, bundleDir, stdoutW, stderrW)
		if err != nil {
			t.Fatalf("runsc create: %v", err)
		}
		afterCreate := time.Now()
		defer c.Destroy()
		startBreakdown, err := startWithNetworkArgs(c, conf, networkArgs)
		if err != nil {
			t.Fatalf("startWithNetworkArgs: %v", err)
		}
		afterStart := time.Now()
		readyAt := netPool.ready.waitFor(t, netEnv.guestIP)
		t.Logf("normal start breakdown: create=%v hostsettings=%v connect=%v network_rpc=%v root_start_rpc=%v app_ready=%v total=%v",
			afterCreate.Sub(start), startBreakdown.hostsettings, startBreakdown.connect, startBreakdown.networkRPC,
			startBreakdown.rootStartRPC, readyAt.Sub(afterStart), readyAt.Sub(start))
		t.Logf("normal start ready: %v", readyAt.Sub(start))
	})

	t.Run("fork", func(t *testing.T) {
		netEnv := netPool.networks[0]
		networkArgs, networkCleanup := netEnv.createNetworkArgs(t)
		defer networkCleanup()

		templateSpec := newReadySpec(rootfsDir, rootfsImage, netPool.hostIP.String(), netPool.ready.port, true)
		_, templateBundleDir, templateCleanup, err := testutil.SetupContainer(templateSpec, conf)
		if err != nil {
			t.Fatalf("SetupContainer(template): %v", err)
		}
		defer templateCleanup()

		templateOutPath, templateOut := openAppendTestFile(t, "template-stdout")
		templateErrPath, templateErr := openAppendTestFile(t, "template-stderr")
		defer templateOut.Close()
		defer templateErr.Close()
		defer logStdioOnFailure(t, templateOutPath, templateErrPath)

		templateID := testutil.RandomContainerID()
		template, err := CreateTemplate(conf, TemplateArgs{
			ID:        templateID,
			Workers:   templateForkE2EWorkerCount(t),
			Spec:      templateSpec,
			BundleDir: templateBundleDir,
			Stdout:    templateOut,
			Stderr:    templateErr,
		})
		if err != nil {
			t.Fatalf("CreateTemplate: %v", err)
		}
		defer DeleteTemplate(template)

		stdoutPath, stdoutW := openAppendTestFile(t, "fork-stdout")
		stderrPath, stderrW := openAppendTestFile(t, "fork-stderr")
		defer stdoutW.Close()
		defer stderrW.Close()
		defer logStdioOnFailure(t, stdoutPath, stderrPath)

		start := time.Now()
		child, err := template.Fork(conf, ForkArgs{
			ID:          testutil.RandomContainerID(),
			BundleDir:   templateBundleDir,
			Stdout:      stdoutW,
			Stderr:      stderrW,
			NetworkArgs: networkArgs,
		})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		afterFork := time.Now()
		defer child.Destroy()
		readyAt := netPool.ready.waitFor(t, netEnv.guestIP)
		t.Logf("fork breakdown: fork_call=%v app_ready=%v total=%v",
			afterFork.Sub(start), readyAt.Sub(afterFork), readyAt.Sub(start))
		t.Logf("fork ready: %v", readyAt.Sub(start))
	})
}

// runTemplateForkTestsInNetNS preserves the Go test runner's selection when
// re-executing the binary in a private network namespace.
func runTemplateForkTestsInNetNS(m *testing.M, marker string) int {
	if os.Getenv(marker) == "1" {
		return m.Run()
	}
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Env = append(os.Environ(), marker+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Cloneflags: unix.CLONE_NEWNET, Pdeathsig: unix.SIGKILL}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "running tests in network namespace: %v\n", err)
		return 1
	}
	return 0
}

func setupTemplateForkE2ERootfs(t testing.TB) (rootfsDir, rootfsImage string) {
	t.Helper()

	testDir, err := os.MkdirTemp(testutil.TmpDir(), "template_fork_e2e_")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testDir) })

	rootfsDir, rootfsImage, err = createReadyEROFS(t, testDir)
	if err != nil {
		t.Fatalf("createReadyEROFS: %v", err)
	}
	return rootfsDir, rootfsImage
}

func createReadyEROFS(t testing.TB, dir string) (rootfsDir, rootfsImage string, err error) {
	t.Helper()

	readyBinary, err := testutil.FindFile("test/cmd/fork_ready/fork_ready")
	if err != nil {
		fallback, statErr := filepath.Abs("bazel-bin/test/cmd/fork_ready/fork_ready")
		if statErr != nil {
			return "", "", err
		}
		if _, statErr := os.Stat(fallback); statErr != nil {
			return "", "", err
		}
		readyBinary = fallback
	}
	rootfsDir = filepath.Join(dir, "ready-rootfs")
	for _, path := range []string{
		rootfsDir,
		filepath.Join(rootfsDir, "dev"),
		filepath.Join(rootfsDir, "proc"),
		filepath.Join(rootfsDir, "sys"),
		filepath.Join(rootfsDir, "tmp"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return "", "", fmt.Errorf("creating %q: %w", path, err)
		}
	}

	guestReadyPath := filepath.Join(rootfsDir, "template_fork_ready")
	if err := copyHostPath(readyBinary, guestReadyPath); err != nil {
		return "", "", fmt.Errorf("copying ready binary: %w", err)
	}
	if err := os.Chmod(guestReadyPath, 0555); err != nil {
		return "", "", fmt.Errorf("chmod ready binary: %w", err)
	}

	rootfsImage = filepath.Join(dir, "ready-rootfs.img")
	if err := createImageEROFS(rootfsImage, rootfsDir, "-E noinline_data"); err != nil {
		return "", "", fmt.Errorf("creating EROFS image: %w", err)
	}
	return rootfsDir, rootfsImage, nil
}

func copyHostPath(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return testutil.Copy(src, dst)
}

func newReadySpec(rootfsDir, rootfsImage, host string, port int, waitReady bool) *specs.Spec {
	spec := newReadyBaseSpec(rootfsDir, rootfsImage, waitReady)
	spec.Process.Env = append(spec.Process.Env,
		"READY_HOST="+host,
		fmt.Sprintf("READY_PORT=%d", port),
	)
	return spec
}

func newReadyBaseSpec(rootfsDir, rootfsImage string, waitReady bool) *specs.Spec {
	spec := testutil.NewSpecWithArgs("/template_fork_ready")
	spec.Root.Path = rootfsDir
	spec.Mounts = nil
	if waitReady {
		spec.Process.Env = append(spec.Process.Env, "WAIT_CHECKPOINT=1")
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations[sentryapi.RootfsPrefix+"type"] = erofs.Name
	spec.Annotations[sentryapi.RootfsPrefix+"source"] = rootfsImage
	spec.Annotations[sentryapi.RootfsPrefix+"overlay"] = config.NoOverlay.String()
	return spec
}

func templateForkE2EConf(t testing.TB) *config.Config {
	t.Helper()
	return templateForkE2EConfForPlatform(t, templateForkE2EPlatform(t))
}

func templateForkE2EConfForPlatform(t testing.TB, platform string) *config.Config {
	t.Helper()
	conf := templateForkE2EBaseConf(t)
	// testutil.TestConfig enables debug, strace, and packet logging by
	// default. Those are useful for functional tests but dominate the
	// ready-path latency this e2e records.
	conf.Debug = false
	conf.DebugLog = ""
	conf.HostSettings = config.HostSettingsIgnore
	conf.LogPackets = false
	conf.Strace = false
	conf.Platform = platform
	conf.Network = config.NetworkNone
	conf.DirectFS = false
	conf.CPUNum = templateForkE2ECPUNum
	if os.Getenv("GVISOR_TEMPLATE_FORK_PRODUCTION") == "1" {
		conf.TestOnlyAllowRunAsCurrentUserWithoutChroot = false
	}
	return conf
}

func templateForkE2EPlatform(t testing.TB) string {
	t.Helper()
	platform := os.Getenv(templateForkE2EPlatformEnv)
	if platform == "" {
		return "kvm"
	}
	switch platform {
	case "kvm", "slimvm":
		return platform
	default:
		t.Fatalf("invalid %s=%q", templateForkE2EPlatformEnv, platform)
		return ""
	}
}

func templateForkE2EDevicePath(t testing.TB, platform string) string {
	t.Helper()
	switch platform {
	case "kvm":
		return "/dev/kvm"
	case "slimvm":
		return "/dev/slimvm"
	default:
		t.Fatalf("unknown platform %q", platform)
		return ""
	}
}

func templateForkE2EBaseConf(t testing.TB) *config.Config {
	t.Helper()
	if tt, ok := t.(*testing.T); ok {
		return testutil.TestConfig(tt)
	}
	logDir := os.TempDir()
	if dir, ok := os.LookupEnv("TEST_UNDECLARED_OUTPUTS_DIR"); ok {
		logDir = dir + "/"
	}
	testFlags := runscflag.NewFlagSet("test", runscflag.ContinueOnError)
	config.RegisterFlags(testFlags)
	conf, err := config.NewFromFlags(testFlags)
	if err != nil {
		t.Fatalf("error loading configuration from flags: %v", err)
	}
	conf.Debug = true
	conf.DebugLog = filepath.Join(logDir, "runsc.log."+t.Name()+".%TIMESTAMP%.%COMMAND%.txt")
	conf.HostSettings = config.HostSettingsIgnore
	conf.LogPackets = true
	conf.Network = config.NetworkNone
	conf.Strace = true
	conf.TestOnlyAllowRunAsCurrentUserWithoutChroot = true
	conf.WatchdogAction = "panic"
	return conf
}

type templateForkE2ENetwork struct {
	guestIP     net.IP
	peerLink    netlink.Link
	networkArgs *sentryapi.CreateLinksAndRoutesArgs
}

type templateForkE2ENetworkPool struct {
	hostIP   net.IP
	ready    *templateForkE2EReadyServer
	networks []templateForkE2ENetwork
}

type templateForkE2EReadyServer struct {
	port     int
	listener *net.TCPListener

	mu      sync.Mutex
	pending map[string][]time.Time
	waiters map[string]chan time.Time
}

func setupTemplateForkE2ENetworkPool(t testing.TB, count int) *templateForkE2ENetworkPool {
	t.Helper()
	if count <= 0 {
		t.Fatalf("network pool count must be positive, got %d", count)
	}
	if count > 253 {
		t.Fatalf("network pool count %d exceeds 198.18.1.0/24 guest capacity", count)
	}

	seq := templateForkE2ENetworkSeq.Add(1)
	bridgeName := fmt.Sprintf("gvbr%x", seq)
	hostIP := net.IPv4(198, 18, 1, 1)
	mask := net.CIDRMask(24, 32)
	bridgeMAC := net.HardwareAddr{0x02, 0x18, 0x01, 0x00, byte(seq), 0x01}

	bridge := createTemplateForkE2EBridge(t, bridgeName, hostIP, mask, bridgeMAC)
	ready := newTemplateForkE2EReadyServer(t, hostIP)

	prefixLen, _ := mask.Size()
	route := sentryapi.Route{
		Destination: net.IPNet{
			IP:   hostIP.Mask(mask),
			Mask: mask,
		},
	}
	pool := &templateForkE2ENetworkPool{
		hostIP:   hostIP,
		ready:    ready,
		networks: make([]templateForkE2ENetwork, 0, count),
	}
	for i := 0; i < count; i++ {
		hostName := fmt.Sprintf("gvh%x%x", seq, i)
		peerName := fmt.Sprintf("gvg%x%x", seq, i)
		guestIP := net.IPv4(198, 18, 1, byte(i+2))
		hostLink, peerLink := createTemplateForkE2EVeth(t, hostName, peerName, bridge)

		networkArgs := &sentryapi.CreateLinksAndRoutesArgs{
			LoopbackLinks: []sentryapi.LoopbackLink{sentryapi.DefaultLoopbackLink},
			FDBasedLinks: []sentryapi.FDBasedLink{
				{
					Name:              peerLink.Attrs().Name,
					InterfaceIndex:    peerLink.Attrs().Index,
					MTU:               peerLink.Attrs().MTU,
					NumChannels:       1,
					LinkAddress:       peerLink.Attrs().HardwareAddr,
					Addresses:         []sentryapi.IPWithPrefix{{Address: guestIP, PrefixLen: prefixLen}},
					Routes:            []sentryapi.Route{route},
					GSOMaxSize:        templateForkE2EGSOMaxSize(peerLink),
					RXChecksumOffload: true,
					QDisc:             config.QDiscFIFO,
					Neighbors: []sentryapi.Neighbor{
						{
							IP:           hostIP,
							HardwareAddr: bridge.Attrs().HardwareAddr,
						},
					},
				},
			},
		}

		t.Logf("e2e network worker %d: bridge=%s(%s), host-veth=%s, guest-veth=%s(%s), port=%d",
			i, bridge.Attrs().Name, hostIP, hostLink.Attrs().Name, peerLink.Attrs().Name, guestIP, ready.port)
		pool.networks = append(pool.networks, templateForkE2ENetwork{
			guestIP:     guestIP,
			peerLink:    peerLink,
			networkArgs: networkArgs,
		})
	}
	return pool
}

func (n templateForkE2ENetwork) createNetworkArgs(t testing.TB) (*sentryapi.CreateLinksAndRoutesArgs, func()) {
	t.Helper()
	deviceFile, err := createTemplateForkE2ERawSocket(n.peerLink)
	if err != nil {
		t.Fatalf("creating raw socket: %v", err)
	}
	args := cloneTemplateForkE2ENetworkArgs(n.networkArgs)
	args.FilePayload = urpc.FilePayload{Files: []*os.File{deviceFile}}
	return args, func() { deviceFile.Close() }
}

func newTemplateForkE2EReadyServer(t testing.TB, hostIP net.IP) *templateForkE2EReadyServer {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: hostIP, Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP(%s): %v", hostIP, err)
	}
	t.Cleanup(func() { listener.Close() })
	server := &templateForkE2EReadyServer{
		port:     listener.Addr().(*net.TCPAddr).Port,
		listener: listener,
		pending:  make(map[string][]time.Time),
		waiters:  make(map[string]chan time.Time),
	}
	go server.serve()
	return server
}

func (s *templateForkE2EReadyServer) serve() {
	for {
		conn, err := s.listener.AcceptTCP()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *templateForkE2EReadyServer) handleConn(conn *net.TCPConn) {
	defer conn.Close()

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(templateForkE2EParallelReadyTimeout))
	const readyMessage = "ready\n"
	var buf [len(readyMessage)]byte
	n, err := io.ReadFull(conn, buf[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ready receiver: peer=%s n=%d err=%v\n", addr.IP, n, err)
		return
	}

	if string(buf[:]) != readyMessage {
		fmt.Fprintf(os.Stderr, "ready receiver: peer=%s unexpected message=%q\n", addr.IP, buf[:])
		return
	}
	readyAt := time.Now()
	// Keep the workload alive until its queued TCP data reaches the receiver.
	if _, err := conn.Write([]byte{'\n'}); err != nil {
		fmt.Fprintf(os.Stderr, "ready acknowledgment: peer=%s err=%v\n", addr.IP, err)
		return
	}
	key := addr.IP.String()
	s.mu.Lock()
	if waiter := s.waiters[key]; waiter != nil {
		delete(s.waiters, key)
		waiter <- readyAt
	} else {
		s.pending[key] = append(s.pending[key], readyAt)
	}
	s.mu.Unlock()
}

func (s *templateForkE2EReadyServer) waitFor(t testing.TB, guestIP net.IP) time.Time {
	t.Helper()
	key := guestIP.String()
	ready := make(chan time.Time, 1)
	s.mu.Lock()
	if pending := s.pending[key]; len(pending) > 0 {
		readyAt := pending[0]
		if len(pending) == 1 {
			delete(s.pending, key)
		} else {
			s.pending[key] = pending[1:]
		}
		s.mu.Unlock()
		return readyAt
	}
	if _, exists := s.waiters[key]; exists {
		s.mu.Unlock()
		t.Fatalf("ready waiter already registered for %s", key)
		return time.Time{}
	}
	s.waiters[key] = ready
	s.mu.Unlock()

	select {
	case readyAt := <-ready:
		return readyAt
	case <-time.After(templateForkE2EParallelReadyTimeout):
		s.mu.Lock()
		delete(s.waiters, key)
		s.mu.Unlock()
		t.Fatalf("timed out waiting for TCP ready from %s", key)
	}
	return time.Time{}
}

func createTemplateForkE2EBridge(t testing.TB, name string, hostIP net.IP, mask net.IPMask, hw net.HardwareAddr) netlink.Link {
	t.Helper()
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(bridge); err != nil {
		t.Fatalf("LinkAdd(bridge): %v", err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkDel(bridge); err != nil {
			t.Logf("LinkDel(%s): %v", name, err)
		}
	})
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", name, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, hw); err != nil {
		t.Fatalf("LinkSetHardwareAddr(%s, %s): %v", name, hw, err)
	}
	link.Attrs().HardwareAddr = hw
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: hostIP, Mask: mask}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("AddrAdd(%s, %s): %v", name, addr, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", name, err)
	}
	return link
}

func createTemplateForkE2EVeth(t testing.TB, hostName, peerName string, bridge netlink.Link) (netlink.Link, netlink.Link) {
	t.Helper()
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("LinkAdd(veth): %v", err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkDel(veth); err != nil {
			t.Logf("LinkDel(%s): %v", hostName, err)
		}
	})

	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", hostName, err)
	}
	peerLink, err := netlink.LinkByName(peerName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", peerName, err)
	}
	if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
		t.Fatalf("LinkSetMaster(%s, %s): %v", hostName, bridge.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(peerLink); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", peerName, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", hostName, err)
	}
	return hostLink, peerLink
}

func createTemplateForkE2ERawSocket(link netlink.Link) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			return nil, fmt.Errorf("requires CAP_NET_RAW to create AF_PACKET socket: %w", err)
		}
		return nil, err
	}
	deviceFile := os.NewFile(uintptr(fd), "template-fork-e2e-raw-device")
	ll := unix.SockaddrLinklayer{
		Protocol: 0x0300, // htons(ETH_P_ALL)
		Ifindex:  link.Attrs().Index,
	}
	if err := unix.Bind(fd, &ll); err != nil {
		deviceFile.Close()
		return nil, fmt.Errorf("binding raw socket to %q: %w", link.Attrs().Name, err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_VNET_HDR, 1); err != nil {
		deviceFile.Close()
		return nil, fmt.Errorf("enabling PACKET_VNET_HDR: %w", err)
	}
	const bufSize = 4 << 20
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, bufSize); err != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, bufSize)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, bufSize); err != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, bufSize)
	}
	return deviceFile, nil
}

func templateForkE2EGSOMaxSize(link netlink.Link) uint32 {
	if gso := link.Attrs().GSOMaxSize; gso != 0 {
		return gso
	}
	return 65536
}

type startRootBreakdown struct {
	hostsettings time.Duration
	connect      time.Duration
	networkRPC   time.Duration
	rootStartRPC time.Duration
}

func startWithNetworkArgs(c *Container, conf *config.Config, networkArgs *sentryapi.CreateLinksAndRoutesArgs) (startRootBreakdown, error) {
	var breakdown startRootBreakdown
	startRoot := func(conf *config.Config, spec *specs.Spec) error {
		start := time.Now()
		if err := hostsettings.Handle(conf); err != nil {
			return fmt.Errorf("host settings: %w (use --host-settings=ignore to bypass)", err)
		}
		breakdown.hostsettings = time.Since(start)

		start = time.Now()
		conn, err := connectSandboxControlForTest(c.Sandbox.GetControlSocketPath())
		if err != nil {
			return err
		}
		defer conn.Close()
		breakdown.connect = time.Since(start)

		start = time.Now()
		if err := conn.Call(sentryapi.ContMgrSetNetworkArgs, networkArgs, nil); err != nil {
			return fmt.Errorf("setting network args: %w", err)
		}
		breakdown.networkRPC = time.Since(start)

		start = time.Now()
		if err := conn.Call(sentryapi.ContMgrRootContainerStart, &c.ID, nil); err != nil {
			return fmt.Errorf("starting root container: %w", err)
		}
		breakdown.rootStartRPC = time.Since(start)
		return nil
	}
	return breakdown, c.startImpl(conf, "start", startRoot, c.Sandbox.StartSubcontainer)
}

func createWithRunscBinary(conf *config.Config, id, bundleDir string, stdout, stderr *os.File) (*Container, error) {
	args := append(conf.ToFlags(), "create", "--bundle", bundleDir, id)
	cmd := exec.Command(specutils.ExePath, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %v: %w", specutils.ExePath, args, err)
	}
	return Load(conf.RootDir, FullID{SandboxID: id, ContainerID: id}, LoadOpts{Exact: true})
}

func connectSandboxControlForTest(path string) (*urpc.Client, error) {
	if path == "" {
		return nil, fmt.Errorf("empty sandbox control socket path")
	}
	if len(path) < linux.UnixPathMax {
		return client.ConnectTo(path)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening long sandbox control socket path %q: %w", path, err)
	}
	defer unix.Close(fd)
	return client.ConnectTo(filepath.Join("/proc/self/fd", strconv.Itoa(fd)))
}

type templateForkE2ESample struct {
	total        time.Duration
	sandboxStart time.Duration
	appReady     time.Duration
}

func templateForkE2EWorkerCount(t testing.TB) int {
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

func createSharedTemplateForkE2ETemplate(t testing.TB, conf *config.Config, rootfsDir, rootfsImage, host string, port, workers int) *Template {
	t.Helper()

	templateSpec := newReadySpec(rootfsDir, rootfsImage, host, port, true)
	_, templateBundleDir, templateCleanup, err := testutil.SetupContainer(templateSpec, conf)
	if err != nil {
		t.Fatalf("SetupContainer(template): %v", err)
	}
	t.Cleanup(templateCleanup)

	templateOutPath, templateOut := openAppendTestFile(t, "template-stdout")
	templateErrPath, templateErr := openAppendTestFile(t, "template-stderr")
	t.Cleanup(func() { templateOut.Close() })
	t.Cleanup(func() { templateErr.Close() })
	t.Cleanup(func() { logStdioOnFailure(t, templateOutPath, templateErrPath) })

	createStart := time.Now()
	template, err := CreateTemplate(conf, TemplateArgs{
		ID:        testutil.RandomContainerID(),
		Workers:   workers,
		Spec:      templateSpec,
		BundleDir: templateBundleDir,
		Stdout:    templateOut,
		Stderr:    templateErr,
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	t.Logf("template lifecycle: create_api=%v", time.Since(createStart))
	t.Cleanup(func() {
		deleteStart := time.Now()
		if err := DeleteTemplate(template); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("DeleteTemplate(%q): %v", template.ID, err)
			}
		}
		t.Logf("template lifecycle: delete_api=%v", time.Since(deleteStart))
	})
	return template
}

func runTemplateForkE2EForkWorker(t testing.TB, netPool *templateForkE2ENetworkPool, template *Template, workerID, iterations int, waitStart func()) []templateForkE2ESample {
	t.Helper()
	conf := templateForkE2EConfForPlatform(t, template.Platform)
	conf.RootDir = template.RootDir
	netEnv := netPool.networks[workerID]
	waitStart()

	samples := make([]templateForkE2ESample, 0, iterations)
	for i := 0; i < iterations; i++ {
		samples = append(samples, runTemplateForkE2EForkOnce(t, conf, netPool, netEnv, template, workerID, i))
	}
	return samples
}

func runTemplateForkE2EForkOnce(t testing.TB, conf *config.Config, netPool *templateForkE2ENetworkPool, netEnv templateForkE2ENetwork, template *Template, workerID, iteration int) templateForkE2ESample {
	t.Helper()

	stdoutPath, stdoutW := openAppendTestFile(t, fmt.Sprintf("fork-%d-%d-stdout", workerID, iteration))
	stderrPath, stderrW := openAppendTestFile(t, fmt.Sprintf("fork-%d-%d-stderr", workerID, iteration))
	defer stdoutW.Close()
	defer stderrW.Close()
	defer logStdioOnFailure(t, stdoutPath, stderrPath)

	start := time.Now()
	networkArgs, networkCleanup := netEnv.createNetworkArgs(t)
	defer networkCleanup()
	child, err := template.Fork(conf, ForkArgs{
		ID:          testutil.RandomContainerID(),
		Stdout:      stdoutW,
		Stderr:      stderrW,
		NetworkArgs: networkArgs,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	ready := false
	defer func() {
		if !ready {
			stacks := make(chan string, 1)
			go func() {
				dump, err := child.Sandbox.Stacks()
				if err != nil {
					dump = fmt.Sprintf("collecting sandbox stacks: %v", err)
				}
				stacks <- dump
			}()
			select {
			case dump := <-stacks:
				t.Logf("sandbox stacks before cleanup:\n%s", dump)
			case <-time.After(5 * time.Second):
				t.Log("timed out collecting sandbox stacks")
			}
		}
		if err := child.Destroy(); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	}()
	afterFork := time.Now()
	readyAt := netPool.ready.waitFor(t, netEnv.guestIP)
	ready = true
	return templateForkE2ESample{
		total:        readyAt.Sub(start),
		sandboxStart: afterFork.Sub(start),
		appReady:     readyAt.Sub(afterFork),
	}
}

func cloneTemplateForkE2ENetworkArgs(src *sentryapi.CreateLinksAndRoutesArgs) *sentryapi.CreateLinksAndRoutesArgs {
	if src == nil {
		return nil
	}
	dst := *src
	dst.FilePayload.Files = append([]*os.File(nil), src.FilePayload.Files...)
	dst.LoopbackLinks = append([]sentryapi.LoopbackLink(nil), src.LoopbackLinks...)
	dst.FDBasedLinks = append([]sentryapi.FDBasedLink(nil), src.FDBasedLinks...)
	for i := range dst.FDBasedLinks {
		dst.FDBasedLinks[i].Addresses = append([]sentryapi.IPWithPrefix(nil), src.FDBasedLinks[i].Addresses...)
		dst.FDBasedLinks[i].Routes = append([]sentryapi.Route(nil), src.FDBasedLinks[i].Routes...)
		dst.FDBasedLinks[i].Neighbors = append([]sentryapi.Neighbor(nil), src.FDBasedLinks[i].Neighbors...)
	}
	dst.XDPLinks = append([]sentryapi.XDPLink(nil), src.XDPLinks...)
	return &dst
}

func percentileDuration(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := (p*len(sorted) + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

func logStdioOnFailure(t testing.TB, stdoutPath, stderrPath string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	if s := readFileForLog(t, stdoutPath); s != "" {
		t.Logf("stdout:\n%s", s)
	}
	if s := readFileForLog(t, stderrPath); s != "" {
		t.Logf("stderr:\n%s", s)
	}
}

// TestTemplateForkSoak exercises concurrent forks across multiple template
// pools. Set GVISOR_TEMPLATE_FORK_SOAK to the minimum duration; every run also
// requires at least 10,000 successful forks.
func TestTemplateForkSoak(t *testing.T) {
	value := os.Getenv("GVISOR_TEMPLATE_FORK_SOAK")
	if value == "" {
		t.Skip("set GVISOR_TEMPLATE_FORK_SOAK to enable the soak test")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		t.Fatalf("invalid soak duration %q", value)
	}
	log.SetLevel(log.Warning)
	platform := templateForkE2EPlatform(t)
	rootfsDir, rootfsImage := setupTemplateForkE2ERootfs(t)
	pool := setupTemplateForkE2ENetworkPool(t, 100)
	confs := []*config.Config{
		templateForkE2EConfForPlatform(t, platform),
		templateForkE2EConfForPlatform(t, platform),
	}
	templates := make([]*Template, len(confs))
	workers := 4
	if os.Getenv("GVISOR_TEMPLATE_FORK_WORKERS") != "" {
		workers = templateForkE2EWorkerCount(t)
	}
	for i, conf := range confs {
		templates[i] = createSharedTemplateForkE2ETemplate(t, conf, rootfsDir, rootfsImage, pool.hostIP.String(), pool.ready.port, workers)
	}
	start := time.Now()
	completed := 0
	levels := []int{10, 20, 50, 100}
	for batch := 0; time.Since(start) < duration || completed < 10000; batch++ {
		concurrency := levels[batch%len(levels)]
		var successes atomic.Int64
		if !t.Run(fmt.Sprintf("batch_%05d_c%d", batch, concurrency), func(t *testing.T) {
			var wg sync.WaitGroup
			for worker := 0; worker < concurrency; worker++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					idx := worker % len(templates)
					samples := runTemplateForkE2EForkWorker(t, pool, templates[idx], worker, 5, func() {})
					successes.Add(int64(len(samples)))
				}(worker)
			}
			wg.Wait()
		}) {
			t.FailNow()
		}
		if got, want := int(successes.Load()), concurrency*5; got != want {
			t.Fatalf("successful forks = %d, want %d", got, want)
		}
		completed += int(successes.Load())
		t.Logf("soak platform=%s elapsed=%s successes=%d concurrency=%d", platform, time.Since(start).Round(time.Second), completed, concurrency)
	}
	t.Logf("soak complete: platform=%s elapsed=%s successful_forks=%d", platform, time.Since(start), completed)
}

// TestTemplateForkFirstUse repeatedly creates a template and exercises its first
// child. Each subtest releases its template and donated files before the next
// iteration, while the network fixture remains shared.
func TestTemplateForkFirstUse(t *testing.T) {
	value := os.Getenv("GVISOR_TEMPLATE_FORK_FIRST_USE")
	if value == "" {
		t.Skip("set GVISOR_TEMPLATE_FORK_FIRST_USE to enable template churn")
	}
	count, err := strconv.Atoi(value)
	if err != nil || count <= 0 {
		t.Fatalf("invalid template count %q", value)
	}
	log.SetLevel(log.Warning)
	rootfsDir, rootfsImage := setupTemplateForkE2ERootfs(t)
	pool := setupTemplateForkE2ENetworkPool(t, 1)
	conf := templateForkE2EConfForPlatform(t, templateForkE2EPlatform(t))
	for i := 0; i < count; i++ {
		if !t.Run(fmt.Sprintf("template_%05d", i), func(t *testing.T) {
			template := createSharedTemplateForkE2ETemplate(t, conf, rootfsDir, rootfsImage, pool.hostIP.String(), pool.ready.port, templateForkE2EWorkerCount(t))
			runTemplateForkE2EForkOnce(t, conf, pool, pool.networks[0], template, 0, i)
		}) {
			t.FailNow()
		}
	}
}
