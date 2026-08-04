//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	fsctlLockVolume   = 0x00090018
	fsctlUnlockVolume = 0x0009001C
)

func tryLockVolumeForWrite(volumePath string, fh *os.File) (func(), error) {
	// Only raw volume/device paths need FSCTL locking. Regular image files do not.
	if !isRawWindowsVolumePath(volumePath) {
		return func() {}, nil
	}

	if fh == nil {
		return nil, fmt.Errorf("file handle is nil")
	}

	h := syscall.Handle(fh.Fd())
	if err := deviceIoControlNoBuffers(h, fsctlLockVolume); err != nil {
		return nil, fmt.Errorf("FSCTL_LOCK_VOLUME failed for %q: %w", volumePath, err)
	}

	return func() {
		_ = deviceIoControlNoBuffers(h, fsctlUnlockVolume)
	}, nil
}

func isRawWindowsVolumePath(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	return strings.HasPrefix(p, `\\.\`)
}

func deviceIoControlNoBuffers(handle syscall.Handle, controlCode uint32) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	deviceIoControl := kernel32.NewProc("DeviceIoControl")

	var bytesReturned uint32
	r1, _, e1 := deviceIoControl.Call(
		uintptr(handle),
		uintptr(controlCode),
		0,
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&bytesReturned)),
		0,
	)
	if r1 == 0 {
		if e1 != syscall.Errno(0) {
			return e1
		}
		return syscall.EINVAL
	}

	return nil
}
