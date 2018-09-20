// Copyright 2018 Google Inc.
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
	"errors"
	"fmt"
	"os"
	"path"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.googlesource.com/gvisor/pkg/abi/linux"
	"gvisor.googlesource.com/gvisor/pkg/control/server"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/pkg/sentry/arch"
	"gvisor.googlesource.com/gvisor/pkg/sentry/control"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel"
	"gvisor.googlesource.com/gvisor/pkg/sentry/socket/epsocket"
	"gvisor.googlesource.com/gvisor/pkg/sentry/state"
	"gvisor.googlesource.com/gvisor/pkg/sentry/time"
	"gvisor.googlesource.com/gvisor/pkg/sentry/watchdog"
	"gvisor.googlesource.com/gvisor/pkg/syserror"
	"gvisor.googlesource.com/gvisor/pkg/urpc"
)

const (
	// ContainerCheckpoint checkpoints a container.
	ContainerCheckpoint = "containerManager.Checkpoint"

	// ContainerDestroy is used to stop a non-root container and free all
	// associated resources in the sandbox.
	ContainerDestroy = "containerManager.Destroy"

	// ContainerEvent is the URPC endpoint for getting stats about the
	// container used by "runsc events".
	ContainerEvent = "containerManager.Event"

	// ContainerExecuteAsync is the URPC endpoint for executing a command in a
	// container..
	ContainerExecuteAsync = "containerManager.ExecuteAsync"

	// ContainerPause pauses the container.
	ContainerPause = "containerManager.Pause"

	// ContainerProcesses is the URPC endpoint for getting the list of
	// processes running in a container.
	ContainerProcesses = "containerManager.Processes"

	// ContainerRestore restores a container from a statefile.
	ContainerRestore = "containerManager.Restore"

	// ContainerResume unpauses the paused container.
	ContainerResume = "containerManager.Resume"

	// ContainerSignal is used to send a signal to a container.
	ContainerSignal = "containerManager.Signal"

	// ContainerStart is the URPC endpoint for running a non-root container
	// within a sandbox.
	ContainerStart = "containerManager.Start"

	// ContainerWait is used to wait on the init process of the container
	// and return its ExitStatus.
	ContainerWait = "containerManager.Wait"

	// ContainerWaitForLoader blocks until the container's loader has been created.
	ContainerWaitForLoader = "containerManager.WaitForLoader"

	// ContainerWaitPID is used to wait on a process with a certain PID in
	// the sandbox and return its ExitStatus.
	ContainerWaitPID = "containerManager.WaitPID"

	// NetworkCreateLinksAndRoutes is the URPC endpoint for creating links
	// and routes in a network stack.
	NetworkCreateLinksAndRoutes = "Network.CreateLinksAndRoutes"

	// RootContainerStart is the URPC endpoint for starting a new sandbox
	// with root container.
	RootContainerStart = "containerManager.StartRoot"

	// SandboxStacks collects sandbox stacks for debugging.
	SandboxStacks = "debug.Stacks"
)

// ControlSocketAddr generates an abstract unix socket name for the given id.
func ControlSocketAddr(id string) string {
	return fmt.Sprintf("\x00runsc-sandbox.%s", id)
}

// controller holds the control server, and is used for communication into the
// sandbox.
type controller struct {
	// srv is the control server.
	srv *server.Server

	// manager holds the containerManager methods.
	manager *containerManager
}

// newController creates a new controller and starts it listening.
func newController(fd int, k *kernel.Kernel, w *watchdog.Watchdog) (*controller, error) {
	srv, err := server.CreateFromFD(fd)
	if err != nil {
		return nil, err
	}

	manager := &containerManager{
		startChan:         make(chan struct{}),
		startResultChan:   make(chan error),
		loaderCreatedChan: make(chan struct{}),
	}
	srv.Register(manager)

	if eps, ok := k.NetworkStack().(*epsocket.Stack); ok {
		net := &Network{
			Stack: eps.Stack,
		}
		srv.Register(net)
	}

	srv.Register(&debug{})

	if err := srv.StartServing(); err != nil {
		return nil, err
	}

	return &controller{
		srv:     srv,
		manager: manager,
	}, nil
}

// containerManager manages sandboes containers.
type containerManager struct {
	// startChan is used to signal when the root container process should
	// be started.
	startChan chan struct{}

	// startResultChan is used to signal when the root container  has
	// started. Any errors encountered during startup will be sent to the
	// channel. A nil value indicates success.
	startResultChan chan error

	// l is the loader that creates containers and sandboxes.
	l *Loader

	// loaderCreatedChan is used to signal when the loader has been created.
	// After a loader is created, a notify method is called that writes to
	// this channel.
	loaderCreatedChan chan struct{}
}

// StartRoot will start the root container process.
func (cm *containerManager) StartRoot(cid *string, _ *struct{}) error {
	log.Debugf("containerManager.StartRoot")
	// Tell the root container to start and wait for the result.
	cm.startChan <- struct{}{}
	if err := <-cm.startResultChan; err != nil {
		return fmt.Errorf("failed to start sandbox: %v", err)
	}
	cm.l.setRootContainerID(*cid)
	return nil
}

// Processes retrieves information about processes running in the sandbox.
func (cm *containerManager) Processes(_, out *[]*control.Process) error {
	log.Debugf("containerManager.Processes")
	return control.Processes(cm.l.k, out)
}

// StartArgs contains arguments to the Start method.
type StartArgs struct {
	// Spec is the spec of the container to start.
	Spec *specs.Spec

	// TODO: Separate sandbox and container configs.
	// Config is the runsc-specific configuration for the sandbox.
	Conf *Config

	// CID is the ID of the container to start.
	CID string

	// FilePayload contains, in order:
	//   * stdin, stdout, and stderr.
	//   * the file descriptor over which the sandbox will
	//     request files from its root filesystem.
	urpc.FilePayload
}

// Start runs a created container within a sandbox.
func (cm *containerManager) Start(args *StartArgs, _ *struct{}) error {
	log.Debugf("containerManager.Start: %+v", args)

	// Validate arguments.
	if args == nil {
		return errors.New("start missing arguments")
	}
	if args.Spec == nil {
		return errors.New("start arguments missing spec")
	}
	if args.Conf == nil {
		return errors.New("start arguments missing config")
	}
	if args.CID == "" {
		return errors.New("start argument missing container ID")
	}
	// Prevent CIDs containing ".." from confusing the sentry when creating
	// /containers/<cid> directory.
	// TODO: Once we have multiple independant roots, this
	// check won't be necessary.
	if path.Clean(args.CID) != args.CID {
		return fmt.Errorf("container ID shouldn't contain directory traversals such as \"..\": %q", args.CID)
	}
	if len(args.FilePayload.Files) < 4 {
		return fmt.Errorf("start arguments must contain stdin, stderr, and stdout followed by at least one file for the container root gofer")
	}

	err := cm.l.startContainer(cm.l.k, args.Spec, args.Conf, args.CID, args.FilePayload.Files)
	if err != nil {
		return err
	}
	log.Debugf("Container %q started", args.CID)

	return nil
}

// Destroy stops a container if it is still running and cleans up its
// filesystem.
func (cm *containerManager) Destroy(cid *string, _ *struct{}) error {
	log.Debugf("containerManager.destroy %q", *cid)
	cm.l.mu.Lock()
	defer cm.l.mu.Unlock()

	if tg, ok := cm.l.containerRootTGs[*cid]; ok {
		// Send SIGKILL to threadgroup.
		if err := tg.SendSignal(&arch.SignalInfo{
			Signo: int32(linux.SIGKILL),
			Code:  arch.SignalInfoUser,
		}); err == nil {
			// SIGKILL sent. Now wait for it to exit.
			log.Debugf("Waiting for container process to exit.")
			tg.WaitExited()
			log.Debugf("Container process exited.")
		} else if err != syserror.ESRCH {
			return fmt.Errorf("error sending SIGKILL to container %q: %v", *cid, err)
		}

		// Remove the container thread group from the map.
		delete(cm.l.containerRootTGs, *cid)
	}

	// Clean up the filesystem by unmounting all mounts for this container
	// and deleting the container root directory.

	// First get a reference to the container root directory.
	mns := cm.l.k.RootMountNamespace()
	mnsRoot := mns.Root()
	defer mnsRoot.DecRef()
	ctx := cm.l.rootProcArgs.NewContext(cm.l.k)
	containerRoot := path.Join(ChildContainersDir, *cid)
	containerRootDirent, err := mns.FindInode(ctx, mnsRoot, nil, containerRoot, linux.MaxSymlinkTraversals)
	if err == syserror.ENOENT {
		// Container must have been destroyed already. That's fine.
		return nil
	}
	if err != nil {
		return fmt.Errorf("error finding container root directory %q: %v", containerRoot, err)
	}
	defer containerRootDirent.DecRef()

	// Iterate through all submounts and unmount them. We unmount lazily by
	// setting detach=true, so we can unmount in any order.
	for _, m := range containerRootDirent.Inode.MountSource.Submounts() {
		root := m.Root()
		defer root.DecRef()

		// Do a best-effort unmount by flushing the refs and unmount
		// with "detach only = true".
		log.Debugf("Unmounting container submount %q", root.BaseName())
		m.FlushDirentRefs()
		if err := mns.Unmount(ctx, root, true /* detach only */); err != nil {
			return fmt.Errorf("error unmounting container submount %q: %v", root.BaseName(), err)
		}
	}

	// Unmount the container root itself.
	log.Debugf("Unmounting container root %q", containerRoot)
	containerRootDirent.Inode.MountSource.FlushDirentRefs()
	if err := mns.Unmount(ctx, containerRootDirent, true /* detach only */); err != nil {
		return fmt.Errorf("error unmounting container root mount %q: %v", containerRootDirent.BaseName(), err)
	}

	// Get a reference to the parent directory and remove the root
	// container directory.
	containersDirDirent, err := mns.FindInode(ctx, mnsRoot, nil, ChildContainersDir, linux.MaxSymlinkTraversals)
	if err != nil {
		return fmt.Errorf("error finding containers directory %q: %v", ChildContainersDir, err)
	}
	defer containersDirDirent.DecRef()
	log.Debugf("Deleting container root %q", containerRoot)
	if err := containersDirDirent.RemoveDirectory(ctx, mnsRoot, *cid); err != nil {
		return fmt.Errorf("error removing directory %q: %v", containerRoot, err)
	}

	// We made it!
	log.Debugf("Destroyed container %q", *cid)
	return nil
}

// ExecArgs contains arguments to Execute.
type ExecArgs struct {
	control.ExecArgs

	// CID is the ID of the container to exec in.
	CID string
}

// ExecuteAsync starts running a command on a created or running sandbox. It
// returns the pid of the new process.
func (cm *containerManager) ExecuteAsync(args *ExecArgs, pid *int32) error {
	log.Debugf("containerManager.ExecuteAsync: %+v", args)
	tgid, err := cm.l.executeAsync(&args.ExecArgs, args.CID)
	if err != nil {
		return err
	}
	*pid = int32(tgid)
	return nil
}

// Checkpoint pauses a sandbox and saves its state.
func (cm *containerManager) Checkpoint(o *control.SaveOpts, _ *struct{}) error {
	log.Debugf("containerManager.Checkpoint")
	state := control.State{
		Kernel:   cm.l.k,
		Watchdog: cm.l.watchdog,
	}
	return state.Save(o, nil)
}

// Pause suspends a container.
func (cm *containerManager) Pause(_, _ *struct{}) error {
	cm.l.k.Pause()
	return nil
}

// WaitForLoader blocks until the container's loader has been created.
func (cm *containerManager) WaitForLoader(_, _ *struct{}) error {
	log.Debugf("containerManager.WaitForLoader")
	<-cm.loaderCreatedChan
	return nil
}

// RestoreOpts contains options related to restoring a container's file system.
type RestoreOpts struct {
	// FilePayload contains the state file to be restored, followed by the
	// platform device file if necessary.
	urpc.FilePayload

	// SandboxID contains the ID of the sandbox.
	SandboxID string
}

// Restore loads a container from a statefile.
// The container's current kernel is destroyed, a restore environment is created,
// and the kernel is recreated with the restore state file. The container then sends the
// signal to start.
func (cm *containerManager) Restore(o *RestoreOpts, _ *struct{}) error {
	log.Debugf("containerManager.Restore")

	var specFile, deviceFile *os.File
	switch numFiles := len(o.FilePayload.Files); numFiles {
	case 2:
		// The device file is donated to the platform, so don't Close
		// it here.
		deviceFile = o.FilePayload.Files[1]
		fallthrough
	case 1:
		specFile = o.FilePayload.Files[0]
		defer specFile.Close()
	case 0:
		return fmt.Errorf("at least one file must be passed to Restore")
	default:
		return fmt.Errorf("at most two files may be passed to Restore")
	}

	// Destroy the old kernel and create a new kernel.
	cm.l.k.Pause()
	cm.l.k.Destroy()

	p, err := createPlatform(cm.l.conf, int(deviceFile.Fd()))
	if err != nil {
		return fmt.Errorf("error creating platform: %v", err)
	}
	k := &kernel.Kernel{
		Platform: p,
	}
	cm.l.k = k

	// Set up the restore environment.
	fds := &fdDispenser{fds: cm.l.goferFDs}
	renv, err := createRestoreEnvironment(cm.l.spec, cm.l.conf, fds)
	if err != nil {
		return fmt.Errorf("error creating RestoreEnvironment: %v", err)
	}
	fs.SetRestoreEnvironment(*renv)

	// Prepare to load from the state file.
	networkStack, err := newEmptyNetworkStack(cm.l.conf, k)
	if err != nil {
		return fmt.Errorf("failed to create network: %v", err)
	}
	info, err := o.FilePayload.Files[0].Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("error file was empty")
	}

	// Load the state.
	loadOpts := state.LoadOpts{
		Source: o.FilePayload.Files[0],
	}
	if err := loadOpts.Load(k, p, networkStack); err != nil {
		return err
	}

	// Set timekeeper.
	k.Timekeeper().SetClocks(time.NewCalibratedClocks())

	// Since we have a new kernel we also must make a new watchdog.
	watchdog := watchdog.New(k, watchdog.DefaultTimeout, cm.l.conf.WatchdogAction)

	// Change the loader fields to reflect the changes made when restoring.
	cm.l.k = k
	cm.l.watchdog = watchdog
	cm.l.rootProcArgs = kernel.CreateProcessArgs{}
	cm.l.restore = true

	// Tell the root container to start and wait for the result.
	cm.startChan <- struct{}{}
	if err := <-cm.startResultChan; err != nil {
		return fmt.Errorf("failed to start sandbox: %v", err)
	}
	cm.l.setRootContainerID(o.SandboxID)
	return nil
}

// Resume unpauses a container.
func (cm *containerManager) Resume(_, _ *struct{}) error {
	cm.l.k.Unpause()
	return nil
}

// Wait waits for the init process in the given container.
func (cm *containerManager) Wait(cid *string, waitStatus *uint32) error {
	log.Debugf("containerManager.Wait")
	return cm.l.waitContainer(*cid, waitStatus)
}

// WaitPIDArgs are arguments to the WaitPID method.
type WaitPIDArgs struct {
	// PID is the PID in the container's PID namespace.
	PID int32

	// CID is the container ID.
	CID string

	// ClearStatus determines whether the exit status of the process should
	// be cleared when WaitPID returns.
	ClearStatus bool
}

// WaitPID waits for the process with PID 'pid' in the sandbox.
func (cm *containerManager) WaitPID(args *WaitPIDArgs, waitStatus *uint32) error {
	log.Debugf("containerManager.Wait")
	return cm.l.waitPID(kernel.ThreadID(args.PID), args.CID, args.ClearStatus, waitStatus)
}

// SignalArgs are arguments to the Signal method.
type SignalArgs struct {
	// CID is the container id.
	CID string

	// Signo is the signal to send to the process.
	Signo int32
}

// Signal sends a signal to the init process of the container.
// TODO: Send signal to exec process.
func (cm *containerManager) Signal(args *SignalArgs, _ *struct{}) error {
	log.Debugf("containerManager.Signal")
	return cm.l.signal(args.CID, args.Signo)
}
