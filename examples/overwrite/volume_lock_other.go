//go:build !windows

package main

import "os"

func tryLockVolumeForWrite(_ string, _ *os.File) (func(), error) {
	return func() {}, nil
}
