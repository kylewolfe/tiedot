// Copyright 2011 Evan Shaw. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gommap

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// mmap on Windows is a two-step process.
// First, we call CreateFileMapping to get a handle.
// Then, we call MapviewToFile to get an actual pointer into memory.
// Because we want to emulate a POSIX-style mmap, we don't want to expose
// the handle -- only the pointer. We also want to return only a byte slice,
// not a struct, so it's convenient to manipulate.

// We keep this map so that we can get back the original handle from the memory address.
var handleLock sync.Mutex
var handleMap = map[uintptr]syscall.Handle{}

// Windows mmap always mapes the entire file regardless of the specified length.
func mmap(length int, hfile uintptr) ([]byte, error) {
	flProtect := uint32(syscall.PAGE_READONLY)
	dwDesiredAccess := uint32(syscall.FILE_MAP_READ)
	flProtect = syscall.PAGE_READWRITE
	dwDesiredAccess = syscall.FILE_MAP_WRITE

	h, errno := syscall.CreateFileMapping(syscall.Handle(hfile), nil, flProtect, 0, 0, nil)
	if h == 0 {
		return nil, os.NewSyscallError("CreateFileMapping", errno)
	}

	addr, errno := syscall.MapViewOfFile(h, dwDesiredAccess, 0, 0, 0)
	if addr == 0 {
		return nil, os.NewSyscallError("MapViewOfFile", errno)
	}
	handleLock.Lock()
	handleMap[addr] = h
	handleLock.Unlock()

	m := MMap{}
	dh := m.header()
	dh.Data = addr
	dh.Len = length
	dh.Cap = length

	return m, nil
}

func flush(addr, len uintptr) error {
	errno := syscall.FlushViewOfFile(addr, len)
	return os.NewSyscallError("FlushViewOfFile", errno)
}

func unmap(addr, len uintptr) error {
	flush(addr, len)
	err := syscall.UnmapViewOfFile(addr)
	if err != nil {
		return err
	}

	handleLock.Lock()
	defer handleLock.Unlock()
	handle, ok := handleMap[addr]
	if !ok {
		// should be impossible; we would've errored above
		return errors.New("unknown base address")
	}
	delete(handleMap, addr)

	e := syscall.CloseHandle(syscall.Handle(handle))
	return os.NewSyscallError("CloseHandle", e)
}
