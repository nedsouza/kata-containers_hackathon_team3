//
// Copyright (c) 2019 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

// Copyright 2015-2019 CNI authors
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

package rootless

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/utils"

	"github.com/containernetworking/plugins/pkg/ns"
	"golang.org/x/sys/unix"
)

// Creates a new persistent network namespace and returns an object
// representing that namespace, without switching to it
func NewNS() (ns.NetNS, error) {
	nsRunDir := filepath.Join(GetRootlessDir(), "netns")

	b := make([]byte, 16)
	_, err := rand.Reader.Read(b)
	if err != nil {
		return nil, fmt.Errorf("failed to generate random netns name: %v", err)
	}

	// Create the directory for mounting network namespaces
	// This needs to be a shared mountpoint in case it is mounted in to
	// other namespaces (containers)
	err = utils.MkdirAllWithInheritedOwner(nsRunDir, 0755)
	if err != nil {
		return nil, err
	}

	nsName := fmt.Sprintf("net-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])

	// create an empty file at the mount point
	nsPath := filepath.Join(nsRunDir, nsName)
	mountPointFd, err := os.Create(nsPath)
	if err != nil {
		return nil, err
	}
	if err := mountPointFd.Close(); err != nil {
		return nil, err
	}

	// Ensure the mount point is cleaned up on errors; if the namespace
	// was successfully mounted this will have no effect because the file
	// is in-use
	defer func() {
		_ = os.RemoveAll(nsPath)
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	// do namespace work in a dedicated goroutine, so that we can safely
	// Lock/Unlock OSThread without upsetting the lock/unlock state of
	// the caller of this function
	go (func() {
		defer wg.Done()
		runtime.LockOSThread()
		// Don't unlock. By not unlocking, golang will kill the OS thread when the
		// goroutine is done (for go1.10+)

		threadNsPath := getCurrentThreadNetNSPath()

		var origNS ns.NetNS
		origNS, err = ns.GetNS(threadNsPath)
		if err != nil {
			rootlessLog.Warnf("cannot open current network namespace %s: %q", threadNsPath, err)
			return
		}
		defer func() {
			if err := origNS.Close(); err != nil {
				rootlessLog.Errorf("unable to close namespace: %q", err)
			}
		}()

		// create a new netns on the current thread
		if os.Getuid() == 0 { 
			err = unix.Unshare(unix.CLONE_NEWNET)
			if err != nil {
				rootlessLog.Warnf("cannot create a new network namespace: %q", err)
				return
			}
		} else {
			err = fmt.Errorf("no root permission")
			rootlessLog.Warnf("cannot create a new network namespace without root permissions")
			return 
		}	

		// Put this thread back to the orig ns, since it might get reused (pre go1.10)
		defer func() {
			if err := origNS.Set(); err != nil {
				if IsRootless() && strings.Contains(err.Error(), "operation not permitted") {
					// When running in rootless mode it will fail to re-join
					// the network namespace owned by root on the host.
					return
				}
				rootlessLog.Warnf("unable to reset namespace: %q", err)
			}
		}()

		// bind mount the netns from the current thread (from /proc) onto the
		// mount point. This causes the namespace to persist, even when there
		// are no threads in the ns.
		err = unix.Mount(threadNsPath, nsPath, "none", unix.MS_BIND, "")
		if err != nil {
			err = fmt.Errorf("failed to bind mount ns at %s: %v", nsPath, err)
		}
	})()
	wg.Wait()

	if err != nil {
		unix.Unmount(nsPath, unix.MNT_DETACH)
		return nil, fmt.Errorf("failed to create namespace: %v", err)
	}

	return ns.GetNS(nsPath)
}

// getCurrentThreadNetNSPath copied from pkg/ns
func getCurrentThreadNetNSPath() string {
	// /proc/self/ns/net returns the namespace of the main thread, not
	// of whatever thread this goroutine is running on.  Make sure we
	// use the thread's net namespace since the thread is switching around
	return fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid())
}
