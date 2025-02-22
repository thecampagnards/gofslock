// Copyright 2022 by Dan Jacques. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows
// +build !windows

package fslock

import (
	"fmt"
	"os"
	"sync"

	// Use legacy syscall package because AIX doesn't have syscall defined
	// in golang.org/x/sys/unix.
	"syscall"
)

func lockImpl(l *L) (Handle, error) {
	return globalUnixLockState.lockImpl(l)
}

var globalUnixLockState unixLockState

// unixLockEntry is an entry in a unixLockState represneting a single inode.
type unixLockEntry struct {
	// file is the underlying open file descriptor.
	file *os.File

	// isShared is true if this is a shared lock entry, false otherwise.
	shared bool
	// sharedCount, if "shared" is true, is the number of in-process open shared
	// handles.
	sharedCount uint64
}

// unixLockState maintains an internal state of filesystem locks.
//
// For runtime usage, this is maintained in the global variable,
// globalUnixLockState.
type unixLockState struct {
	sync.RWMutex
	held map[uint64]*unixLockEntry
}

func (uls *unixLockState) lockImpl(l *L) (Handle, error) {
	fd, err := getOrCreateLockFile(l.Path, l.Content)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Close "fd". On success, we'll clear "fd", so this will become a no-op.
		if fd != nil {
			fd.Close()
		}
	}()

	st, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	stat := st.Sys().(*syscall.Stat_t)

	// Do we already have a lock on this file?
	uls.RLock()
	ule := uls.held[stat.Ino]
	uls.RUnlock()

	if ule != nil {
		// If we are requesting an exclusive lock, or if "ule" is held exclusively,
		// then deny the request.
		if !(l.Shared && ule.shared) {
			return nil, ErrLockHeld
		}
	}

	// Attempt to register the lock.
	uls.Lock()
	defer uls.Unlock()

	// Check again, with write lock held.
	if ule := uls.held[stat.Ino]; ule != nil {
		if !(l.Shared && ule.shared) {
			return nil, ErrLockHeld
		}

		// We're requesting a shared lock, and "ule" is shared, so we can grant a
		// handle.
		ule.sharedCount++
		return &unixLockHandle{uls, ule, stat.Ino, true}, nil
	}

	if uls.held == nil {
		uls.held = make(map[uint64]*unixLockEntry)
	}

	ule = &unixLockEntry{
		file:        fd,
		shared:      l.Shared,
		sharedCount: 1, // Ignored for exclusive.
	}
	uls.held[stat.Ino] = ule
	fd = nil // Don't Close in defer().
	return &unixLockHandle{uls, ule, stat.Ino, ule.shared}, nil
}

type unixLockHandle struct {
	uls    *unixLockState
	ule    *unixLockEntry
	ino    uint64
	shared bool
}

func (l *unixLockHandle) Unlock() error {
	if l.uls == nil {
		panic("lock is not held")
	}

	l.uls.Lock()
	defer l.uls.Unlock()

	ule := l.uls.held[l.ino]
	if ule == nil {
		panic(fmt.Errorf("lock for inode %d is not held", l.ino))
	}
	if l.shared {
		if !ule.shared {
			panic(fmt.Errorf("lock for inode %d is not shared, but handle is shared", l.ino))
		}
		ule.sharedCount--
	}

	if !ule.shared || ule.sharedCount == 0 {
		// Last holder of the lock. Clean it up and unregister.
		if err := ule.file.Close(); err != nil {
			return err
		}
		delete(l.uls.held, l.ino)
	}

	// Clear the lock's "uls" field so that future calls to Unlock will fail
	// immediately.
	l.uls = nil
	return nil
}

func (l *unixLockHandle) LockFile() *os.File { return l.ule.file }

func (l *unixLockHandle) PreserveExec() error {
	if _, _, err := syscall.Syscall(syscall.SYS_FCNTL, l.LockFile().Fd(), syscall.F_SETFD, 0); err != syscall.Errno(0x0) {
		return err
	}
	return nil
}

func getOrCreateLockFile(path string, content []byte) (*os.File, error) {
	const mode = 0640 | os.ModeTemporary

	// Loop until we've either created or opened the file.
	for {
		// Attempt to open the file. This will succeed if the file already exists.
		fd, err := os.OpenFile(path, os.O_RDWR, mode)
		switch {
		case err == nil:
			// Successfully opened the file, return handle.
			return fd, nil

		case os.IsNotExist(err):
			// The file doesn't exist. Attempt to exclusively create it.
			//
			// If this fails, the file exists, so we will try opening it again.
			fd, err := os.OpenFile(path, (os.O_CREATE | os.O_EXCL | os.O_RDWR), mode)
			switch {
			case err == nil:
				// Successfully created the new file. If we have content to write, try
				// and write it.
				if len(content) > 0 {
					// Failure to write content is non-fatal.
					_, _ = fd.Write(content)
				}
				return fd, err

			case os.IsExist(err):
				// Loop, we will try to open the file.

			default:
				return nil, err
			}

		default:
			return nil, err
		}
	}
}
