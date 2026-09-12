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
	"io"
	"os"
	"slices"
	"time"

	"github.com/mohae/deepcopy"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

// TemplateBackendFile describes a file-backed tmpfs or overlay backend that
// must be replaced in each sandbox forked from a template.
type TemplateBackendFile struct {
	MountPoint string `json:"mountPoint"`
	MountSrc   string `json:"mountSrc"`
	GoferConf  string `json:"goferConf"`
	AnonDir    string `json:"anonDir,omitempty"`
}

// TemplateState is the template-specific part of persisted Container state.
type TemplateState struct {
	Ready        bool                  `json:"ready"`
	Workers      int                   `json:"workers,omitempty"`
	Platform     string                `json:"platform"`
	BackendFiles []TemplateBackendFile `json:"backendFiles,omitempty"`
}

// Template is a narrow lifecycle view of a Container that has paused after
// user-space initialization and can serve SandboxTemplate.Fork requests.
// Its exported fields describe the seed when the handle was created or loaded.
type Template struct {
	ID      string
	RootDir string

	// PID is the outer sandbox PID namespace's init process. After template
	// initialization this process is the templatestub reaper; terminating it
	// also terminates the nested template and fork child PID namespaces.
	PID int

	Ready    bool
	Platform string

	destroyMu    *sync.Mutex
	container    *Container
	seed         *sandbox.Sandbox
	process      *specs.Process
	backendFiles []TemplateBackendFile
}

// TemplateArgs configures a single-container sandbox template. Initialization
// runs without Sentry seccomp and must execute only trusted code. Subcontainers
// are not supported. After initialization, the application opens
// /proc/gvisor/checkpoint, writes "1", and reads from the same FD until it
// receives "restore\n" in a forked child. Before the checkpoint request, the
// workload must stay in the root network namespace and must not create network
// namespaces or endpoints.
type TemplateArgs struct {
	// Workers is the number of template processes forked from the initialized
	// seed. They share one listening socket. Zero selects one worker.
	// A worker exit terminates the entire pool and its forked sandboxes.
	Workers int

	// ID is the template sandbox ID.
	ID string

	// Spec is the OCI spec used to boot the template.
	Spec *specs.Spec

	// BundleDir is the directory containing the container bundle.
	BundleDir string

	// PIDFile is the filename where the template sandbox PID will be written.
	PIDFile string

	// Stdout and Stderr are used as the template root stdio. They must be
	// writable regular files opened with O_APPEND. If nil, os.Stdout and
	// os.Stderr are used and must satisfy the same constraints. Template stdin
	// is always /dev/null.
	Stdout *os.File
	Stderr *os.File
}

func collectTemplateBackendFiles(conf *config.Config, c *Container, mountHints *boot.PodMountHints, rootfsHint *boot.RootfsHint) ([]TemplateBackendFile, error) {
	if len(c.GoferMountConfs) == 0 {
		return nil, nil
	}
	var backendFiles []TemplateBackendFile
	add := func(mountPoint, mountSrc string, goferConf specutils.GoferMountConf, overlayMedium config.OverlayMedium) error {
		if !goferConf.IsFilestorePresent() {
			return nil
		}
		info := TemplateBackendFile{
			MountPoint: mountPoint,
			MountSrc:   mountSrc,
			GoferConf:  goferConf.String(),
		}
		if goferConf.Upper == specutils.AnonOverlay {
			if !overlayMedium.IsBackedByAnon() {
				return fmt.Errorf("anonymous overlay for %q lost host directory metadata", mountPoint)
			}
			info.AnonDir = overlayMedium.HostFileDir()
		}
		backendFiles = append(backendFiles, info)
		return nil
	}

	ovlConf := conf.GetOverlay2()
	rootOverlay := ovlConf.RootOverlayMedium()
	if rootfsHint != nil {
		rootOverlay = rootfsHint.Overlay
	}
	if c.Spec.Root.Readonly {
		rootOverlay = config.NoOverlay
	}
	if err := add("/", c.Spec.Root.Path, c.GoferMountConfs[0], rootOverlay); err != nil {
		return nil, err
	}

	mountIdx := 1
	for _, m := range c.Spec.Mounts {
		if !specutils.HasMountConfig(m) {
			continue
		}
		if mountIdx >= len(c.GoferMountConfs) {
			return nil, fmt.Errorf("missing gofer mount config for mount %q", m.Destination)
		}
		overlay := ovlConf.SubMountOverlayMedium()
		if specutils.IsReadonlyMount(m.Options) {
			overlay = config.NoOverlay
		}
		if hint := mountHints.FindMount(m.Source); hint != nil && hint.IsSandboxLocal() {
			overlay = config.SelfOverlay
		}
		if err := add(m.Destination, m.Source, c.GoferMountConfs[mountIdx], overlay); err != nil {
			return nil, err
		}
		mountIdx++
	}
	return backendFiles, nil
}

// CreateTemplate boots a sandbox template and waits until it can serve fork
// requests.
func CreateTemplate(conf *config.Config, args TemplateArgs) (*Template, error) {
	if args.Workers < 0 {
		return nil, fmt.Errorf("template workers must be non-negative")
	}
	if args.Workers == 0 {
		args.Workers = 1
	}
	if err := validateID(args.ID); err != nil {
		return nil, err
	}
	if args.Spec == nil {
		return nil, fmt.Errorf("spec must be provided")
	}
	if args.Spec.Process == nil {
		return nil, fmt.Errorf("spec process must be provided")
	}
	if args.Spec.Process.Terminal {
		return nil, fmt.Errorf("sandbox templates do not support terminal stdio")
	}
	if !specutils.IsRootContainer(args.Spec) {
		return nil, fmt.Errorf("sandbox template must be a root container")
	}
	if err := validateTemplateSandboxConfig(conf, args.Spec, "sandbox template"); err != nil {
		return nil, err
	}

	stdout := args.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if err := specutils.ValidateStdioFile(stdout, "template stdout"); err != nil {
		return nil, err
	}
	stderr := args.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	if err := specutils.ValidateStdioFile(stderr, "template stderr"); err != nil {
		return nil, err
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("opening template stdin: %w", err)
	}
	defer stdin.Close()

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating template readiness pipe: %w", err)
	}
	defer readyR.Close()
	defer readyW.Close()
	c, err := New(conf, Args{
		ID:                args.ID,
		Spec:              args.Spec,
		BundleDir:         args.BundleDir,
		PIDFile:           args.PIDFile,
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		TemplateSandbox:   true,
		TemplateWorkers:   args.Workers,
		TemplateReadyFile: readyW,
	})
	if err != nil {
		return nil, fmt.Errorf("creating template container: %w", err)
	}
	readyW.Close()
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = c.destroy(c.Sandbox)
		}
	}()

	if pid := c.GoferPid.Load(); pid != 0 {
		return nil, fmt.Errorf("sandbox templates must be goferless, but gofer PID %d was created", pid)
	}

	mountHints, err := boot.NewPodMountHints(args.Spec)
	if err != nil {
		return nil, fmt.Errorf("creating template mount hints: %w", err)
	}
	rootfsHint, err := boot.NewRootfsHint(args.Spec)
	if err != nil {
		return nil, fmt.Errorf("creating template rootfs hint: %w", err)
	}
	backendFiles, err := collectTemplateBackendFiles(conf, c, mountHints, rootfsHint)
	if err != nil {
		return nil, fmt.Errorf("collecting template backend files: %w", err)
	}
	c.TemplateState = &TemplateState{
		Platform:     conf.Platform,
		Workers:      args.Workers,
		BackendFiles: backendFiles,
	}

	if err := c.Start(conf); err != nil {
		return nil, fmt.Errorf("starting template container: %w", err)
	}
	if err := readyR.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return nil, err
	}
	var ready [1]byte
	if _, err := io.ReadFull(readyR, ready[:]); err != nil {
		return nil, fmt.Errorf("waiting for sandbox template: %w", err)
	}
	if err := c.modify(func() error {
		if path := c.Sandbox.GetControlSocketPath(); path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing template control socket %q: %w", path, err)
			}
		}
		c.Sandbox.ControlSocketPath = ""
		c.TemplateState.Ready = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("marking template ready: %w", err)
	}

	cleanupNeeded = false
	return templateFromContainer(c), nil
}

func validateTemplateForkConfig(conf *config.Config, name string) error {
	if conf.DisableSeccomp {
		return fmt.Errorf("%s requires Sentry seccomp", name)
	}
	if conf.Platform != "kvm" && conf.Platform != "slimvm" {
		return fmt.Errorf("%s requires --platform=kvm or --platform=slimvm (got %q)", name, conf.Platform)
	}
	if conf.Network != config.NetworkSandbox && conf.Network != config.NetworkNone {
		return fmt.Errorf("%s requires --network=sandbox or --network=none (got %v)", name, conf.Network)
	}
	if conf.DirectFS {
		return fmt.Errorf("%s is incompatible with --directfs", name)
	}
	if conf.Rootless {
		return fmt.Errorf("%s is incompatible with --rootless", name)
	}
	if conf.MetricServer != "" {
		return fmt.Errorf("%s does not support metric server registration", name)
	}
	return nil
}

func validateTemplateSandboxConfig(conf *config.Config, spec *specs.Spec, name string) error {
	if err := validateTemplateForkConfig(conf, name); err != nil {
		return err
	}
	rootfsHint, err := boot.NewRootfsHint(spec)
	if err != nil {
		return fmt.Errorf("parsing rootfs hint: %w", err)
	}
	if rootfsHint == nil || rootfsHint.Mount.Type != "erofs" {
		return fmt.Errorf("%s requires an EROFS rootfs (set the %q spec annotation to %q and %q to the image path)",
			name, boot.RootfsPrefix+"type", "erofs", boot.RootfsPrefix+"source")
	}
	return nil
}

func templateFromContainer(c *Container) *Template {
	t := &Template{
		ID:        c.ID,
		RootDir:   c.Saver.RootDir,
		PID:       c.SandboxPid(),
		destroyMu: new(sync.Mutex),
		container: c,
		seed:      c.Sandbox,
		process:   deepcopy.Copy(c.Spec.Process).(*specs.Process),
	}
	if c.TemplateState != nil {
		t.Ready = c.TemplateState.Ready
		t.Platform = c.TemplateState.Platform
		t.backendFiles = slices.Clone(c.TemplateState.BackendFiles)
	}
	return t
}

// ListTemplates lists all sandbox templates tracked in rootDir.
func ListTemplates(rootDir string) ([]Template, error) {
	ids, err := listMatch(rootDir, FullID{})
	if err != nil {
		return nil, err
	}
	var templates []Template
	for _, id := range ids {
		if template, _ := isTemplateState(rootDir, id); !template {
			continue
		}
		t, err := LoadTemplate(rootDir, id.ContainerID)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Warningf("Skipping template %q while listing templates: %v", id, err)
			}
			continue
		}
		templates = append(templates, *t)
	}
	return templates, nil
}

// LoadTemplate loads a sandbox template from persisted Container state.
func LoadTemplate(rootDir, id string) (*Template, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	c, err := Load(rootDir, FullID{SandboxID: id, ContainerID: id}, LoadOpts{
		Exact:         true,
		SkipCheck:     true,
		AllowTemplate: true,
	})
	if err != nil {
		return nil, err
	}
	if c.Sandbox == nil || !c.Sandbox.Template {
		_ = c.Saver.close()
		return nil, fmt.Errorf("container %q is not a sandbox template", id)
	}
	return templateFromContainer(c), nil
}

func (c *Container) isTemplateGeneration(seed *sandbox.Sandbox, saved *Container) bool {
	return saved.Sandbox != nil && saved.Sandbox.Template &&
		saved.Sandbox.TemplateSocketID == seed.TemplateSocketID && saved.CreatedAt.Equal(c.CreatedAt)
}

// Destroy terminates the template process tree and removes its Container state
// and template socket.
func (t *Template) Destroy() error {
	if t == nil || t.destroyMu == nil {
		return nil
	}
	t.destroyMu.Lock()
	defer t.destroyMu.Unlock()
	if t.container == nil {
		return nil
	}
	if err := t.container.destroy(t.seed); err != nil {
		return err
	}
	t.container = nil
	return nil
}

// DeleteTemplate destroys template. Passing the object returned by
// CreateTemplate preserves Container child ownership and reaps the template
// stub in the creating process.
func DeleteTemplate(template *Template) error {
	if template == nil {
		return fmt.Errorf("template must be provided")
	}
	return template.Destroy()
}
