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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	runscflag "gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
)

var (
	templateForkStartupConcurrency = runscflag.String("template-fork-startup-concurrency", "1,10,20", "comma-separated startup benchmark concurrency levels")
	templateForkStartupIterations  = runscflag.Int("template-fork-startup-iterations", templateForkE2EIterationsPerWorker, "startup benchmark launches per worker")
	templateForkStartupWarmup      = runscflag.Int("template-fork-startup-warmup", 3, "untimed warmup launches per worker")
	templateForkStartupModes       = runscflag.String("template-fork-startup-modes", "start_args,fork_args", "comma-separated startup benchmark modes: start_args, fork_args")
	templateForkStartupPlatform    = runscflag.String("template-fork-startup-platform", "kvm", "startup benchmark platform: kvm or slimvm")
	templateForkStartupWorkers     = runscflag.Int("template-fork-startup-workers", 1, "template workers forked from one initialized seed")
	templateForkStartupCPUNum      = runscflag.Int("template-fork-startup-cpu-num", 1, "number of sandbox CPUs for the startup benchmark")
)

const templateForkStartupNetNSEnv = "GVISOR_TEMPLATE_FORK_STARTUP_NETNS"

func TestMain(m *testing.M) {
	config.RegisterFlags(runscflag.CommandLine)
	_ = runscflag.CommandLine.Set("test.benchtime", "1x")
	ensureTemplateForkStartupTestArg("test.run", "^$")
	ensureTemplateForkStartupTestArg("test.bench", "BenchmarkTemplateForkStartup")
	log.SetTarget(templateForkStartupDiscardEmitter{})
	runscflag.Parse()
	if err := testutil.ConfigureExePath(); err != nil {
		panic(err.Error())
	}
	if err := specutils.MaybeRunAsRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running as root: %v", err)
		os.Exit(123)
	}
	os.Exit(runTemplateForkTestsInNetNS(m, templateForkStartupNetNSEnv))
}

type templateForkStartupDiscardEmitter struct{}

func (templateForkStartupDiscardEmitter) Emit(_ int, _ log.Level, _ time.Time, _ string, _ ...any) {}

func BenchmarkTemplateForkStartup(b *testing.B) {
	skipIfNotAvailable(b, "mkfs.erofs")
	platform := parseTemplateForkStartupPlatform(b)
	templateForkE2ECPUNum = parseTemplateForkStartupCPUNum(b)
	if *templateForkStartupWorkers < 1 {
		b.Fatal("template workers must be positive")
	}
	if dev := templateForkE2EDevicePath(b, platform); dev != "" {
		if _, err := os.Stat(dev); err != nil {
			b.Fatalf("selected platform %q requires %s: %v", platform, dev, err)
		}
	}
	concurrencyCases := parseTemplateForkStartupConcurrency(b)
	modes := parseTemplateForkStartupModes(b)
	for _, concurrency := range concurrencyCases {
		concurrency := concurrency
		b.Run(fmt.Sprintf("concurrency_%02d", concurrency), func(b *testing.B) {
			for _, mode := range modes {
				mode := mode
				b.Run(mode, func(b *testing.B) {
					switch mode {
					case "start_args":
						benchmarkTemplateForkStartupStartArgs(b, platform, concurrency)
					case "fork_args":
						benchmarkTemplateForkStartupForkArgs(b, platform, concurrency)
					default:
						b.Fatalf("unknown startup benchmark mode %q", mode)
					}
				})
			}
		})
	}
}

func parseTemplateForkStartupPlatform(tb testing.TB) string {
	tb.Helper()
	switch platform := strings.TrimSpace(*templateForkStartupPlatform); platform {
	case "kvm", "slimvm":
		return platform
	default:
		tb.Fatalf("invalid -template-fork-startup-platform value %q", platform)
		return ""
	}
}

func parseTemplateForkStartupCPUNum(tb testing.TB) int {
	tb.Helper()
	if *templateForkStartupCPUNum <= 0 {
		tb.Fatalf("-template-fork-startup-cpu-num must be positive, got %d", *templateForkStartupCPUNum)
	}
	return *templateForkStartupCPUNum
}

func ensureTemplateForkStartupTestArg(name, value string) {
	prefixes := []string{"-" + name, "--" + name}
	for i, arg := range os.Args[1:] {
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, prefix+"=") {
				if strings.TrimPrefix(arg, prefix+"=") == "" {
					os.Args[i+1] = prefix + "=" + value
				}
				return
			}
			if arg == prefix {
				return
			}
		}
	}
	os.Args = append(os.Args, "-"+name+"="+value)
}

func benchmarkTemplateForkStartupStartArgs(b *testing.B, platform string, concurrency int) {
	b.StopTimer()
	rootfsDir, rootfsImage := setupTemplateForkE2ERootfs(b)
	netPool := setupTemplateForkE2ENetworkPool(b, concurrency)
	runTemplateForkStartupParallel(b, "start_args", concurrency, func(tb testing.TB, workerID, iterations int, waitStart func()) []templateForkE2ESample {
		return runTemplateForkStartupStartArgsWorker(tb, platform, rootfsDir, rootfsImage, netPool, workerID, iterations, waitStart)
	})
}

func benchmarkTemplateForkStartupForkArgs(b *testing.B, platform string, concurrency int) {
	b.StopTimer()
	rootfsDir, rootfsImage := setupTemplateForkE2ERootfs(b)
	netPool := setupTemplateForkE2ENetworkPool(b, concurrency)

	templateConf := templateForkE2EConfForPlatform(b, platform)
	template := createSharedTemplateForkE2ETemplate(b, templateConf, rootfsDir, rootfsImage, netPool.hostIP.String(), netPool.ready.port, *templateForkStartupWorkers)

	runTemplateForkStartupParallel(b, "fork_args", concurrency, func(tb testing.TB, workerID, iterations int, waitStart func()) []templateForkE2ESample {
		return runTemplateForkE2EForkWorker(tb, netPool, template, workerID, iterations, waitStart)
	})
}

func parseTemplateForkStartupConcurrency(tb testing.TB) []int {
	tb.Helper()
	var out []int
	for _, s := range strings.Split(*templateForkStartupConcurrency, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			tb.Fatalf("invalid -template-fork-startup-concurrency value %q", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		tb.Fatalf("-template-fork-startup-concurrency must contain at least one positive value")
	}
	return out
}

func parseTemplateForkStartupModes(tb testing.TB) []string {
	tb.Helper()
	valid := map[string]bool{
		"start_args": true,
		"fork_args":  true,
	}
	var out []string
	for _, mode := range strings.Split(*templateForkStartupModes, ",") {
		mode = strings.TrimSpace(mode)
		if mode == "" {
			continue
		}
		if !valid[mode] {
			tb.Fatalf("invalid -template-fork-startup-modes value %q", mode)
		}
		out = append(out, mode)
	}
	if len(out) == 0 {
		tb.Fatalf("-template-fork-startup-modes must contain at least one mode")
	}
	return out
}

func runTemplateForkStartupParallel(b *testing.B, name string, concurrency int, runWorker func(testing.TB, int, int, func()) []templateForkE2ESample) {
	b.Helper()
	iterations := *templateForkStartupIterations
	if iterations <= 0 {
		b.Fatalf("-template-fork-startup-iterations must be positive, got %d", iterations)
	}
	warmup := *templateForkStartupWarmup
	if warmup < 0 {
		b.Fatal("-template-fork-startup-warmup must not be negative")
	}
	total := iterations * concurrency

	ready := make(chan struct{}, concurrency)
	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		samples []templateForkE2ESample
	)

	for workerID := 0; workerID < concurrency; workerID++ {
		workerID := workerID
		wg.Add(1)
		go func() {
			defer wg.Done()
			var readySent atomic.Bool
			defer func() {
				if !readySent.Swap(true) {
					ready <- struct{}{}
				}
			}()
			if warmup != 0 {
				runWorker(b, workerID, warmup, func() {})
			}
			local := runWorker(b, workerID, iterations, func() {
				if !readySent.Swap(true) {
					ready <- struct{}{}
				}
				<-start
			})
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}()
	}

	for i := 0; i < concurrency; i++ {
		<-ready
	}
	b.ResetTimer()
	startTime := time.Now()
	close(start)
	wg.Wait()
	endTime := time.Now()
	elapsed := endTime.Sub(startTime)
	b.StopTimer()
	b.Logf("measurement interval: benchmark=%s start_unix_ns=%d end_unix_ns=%d", b.Name(), startTime.UnixNano(), endTime.UnixNano())
	reportTemplateForkStartupStats(b, name, samples, total, concurrency, elapsed)
}

func reportTemplateForkStartupStats(b *testing.B, name string, samples []templateForkE2ESample, total, concurrency int, elapsed time.Duration) {
	b.Helper()
	if len(samples) != total {
		b.Fatalf("%s collected %d samples, want %d", name, len(samples), total)
	}
	// Both endpoints use the launch start; readiness may precede API return.
	readyStats := templateForkStartupDurationStats(samples, func(sample templateForkE2ESample) time.Duration { return sample.total })
	apiStats := templateForkStartupDurationStats(samples, func(sample templateForkE2ESample) time.Duration { return sample.sandboxStart })

	b.ReportMetric(float64(total)/elapsed.Seconds(), "launches/s")
	b.ReportMetric(durationMillis(readyStats.avg), "app_ready_avg_ms")
	b.ReportMetric(durationMillis(readyStats.p50), "app_ready_p50_ms")
	b.ReportMetric(durationMillis(readyStats.p90), "app_ready_p90_ms")
	b.ReportMetric(durationMillis(readyStats.p95), "app_ready_p95_ms")
	b.ReportMetric(durationMillis(readyStats.p99), "app_ready_p99_ms")
	b.ReportMetric(durationMillis(apiStats.avg), "api_return_avg_ms")
	b.ReportMetric(durationMillis(apiStats.p50), "api_return_p50_ms")
	b.ReportMetric(durationMillis(apiStats.p90), "api_return_p90_ms")
	b.ReportMetric(durationMillis(apiStats.p95), "api_return_p95_ms")
	b.ReportMetric(durationMillis(apiStats.p99), "api_return_p99_ms")
	b.Logf("%s startup summary: total=%d concurrency=%d elapsed=%v throughput=%.2f/s",
		name, total, concurrency, elapsed, float64(total)/elapsed.Seconds())
	b.Logf("%s app_ready: avg=%v p50=%v p90=%v min=%v max=%v",
		name, readyStats.avg, readyStats.p50, readyStats.p90, readyStats.min, readyStats.max)
	b.Logf("%s api_return: avg=%v p50=%v p90=%v min=%v max=%v",
		name, apiStats.avg, apiStats.p50, apiStats.p90, apiStats.min, apiStats.max)
}

type templateForkStartupStats struct {
	avg time.Duration
	p50 time.Duration
	p90 time.Duration
	p95 time.Duration
	p99 time.Duration
	min time.Duration
	max time.Duration
}

func templateForkStartupDurationStats(samples []templateForkE2ESample, pick func(templateForkE2ESample) time.Duration) templateForkStartupStats {
	values := make([]time.Duration, 0, len(samples))
	var sum time.Duration
	for _, sample := range samples {
		value := pick(sample)
		values = append(values, value)
		sum += value
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return templateForkStartupStats{
		avg: sum / time.Duration(len(values)),
		p50: percentileDuration(values, 50),
		p90: percentileDuration(values, 90),
		p95: percentileDuration(values, 95),
		p99: percentileDuration(values, 99),
		min: values[0],
		max: values[len(values)-1],
	}
}

func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func runTemplateForkStartupStartArgsWorker(tb testing.TB, platform, rootfsDir, rootfsImage string, netPool *templateForkE2ENetworkPool, workerID, iterations int, waitStart func()) []templateForkE2ESample {
	tb.Helper()
	conf := templateForkE2EConfForPlatform(tb, platform)
	netEnv := netPool.networks[workerID]
	waitStart()

	samples := make([]templateForkE2ESample, 0, iterations)
	for i := 0; i < iterations; i++ {
		samples = append(samples, runTemplateForkStartupStartArgsOnce(tb, conf, rootfsDir, rootfsImage, netPool, netEnv, workerID, i))
	}
	return samples
}

func runTemplateForkStartupStartArgsOnce(tb testing.TB, conf *config.Config, rootfsDir, rootfsImage string, netPool *templateForkE2ENetworkPool, netEnv templateForkE2ENetwork, workerID, iteration int) templateForkE2ESample {
	tb.Helper()
	spec := newReadySpec(rootfsDir, rootfsImage, netPool.hostIP.String(), netPool.ready.port, false)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		tb.Fatalf("SetupContainer: %v", err)
	}
	defer cleanup()

	stdoutPath, stdoutW := openAppendTestFile(tb, fmt.Sprintf("start-args-%d-%d-stdout", workerID, iteration))
	stderrPath, stderrW := openAppendTestFile(tb, fmt.Sprintf("start-args-%d-%d-stderr", workerID, iteration))
	defer stdoutW.Close()
	defer stderrW.Close()
	defer logStdioOnFailure(tb, stdoutPath, stderrPath)

	start := time.Now()
	c, err := createWithRunscBinary(conf, testutil.RandomContainerID(), bundleDir, stdoutW, stderrW)
	if err != nil {
		tb.Fatalf("runsc create: %v", err)
	}
	defer func() {
		if err := c.Destroy(); err != nil {
			tb.Fatalf("Destroy: %v", err)
		}
	}()
	networkArgs, networkCleanup := netEnv.createNetworkArgs(tb)
	defer networkCleanup()
	if _, err := startWithNetworkArgs(c, conf, networkArgs); err != nil {
		tb.Fatalf("startWithNetworkArgs: %v", err)
	}
	afterStart := time.Now()
	readyAt := netPool.ready.waitFor(tb, netEnv.guestIP)
	return templateForkE2ESample{
		total:        readyAt.Sub(start),
		sandboxStart: afterStart.Sub(start),
		appReady:     readyAt.Sub(afterStart),
	}
}
