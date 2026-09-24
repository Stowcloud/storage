//go:build windows

package s3

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"github.com/stowcloud/storage"
	"golang.org/x/sys/windows"
)

func spaceCapacity(path string) (storage.Space, error) {
	root := filepath.VolumeName(path) + `\`
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return storage.Space{}, fmt.Errorf("s3: scratch volume: %w", err)
	}
	var free, total, available uint64
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	if err := proc.Find(); err != nil {
		return storage.Space{}, fmt.Errorf("s3: scratch capacity: %w", err)
	}
	ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&available)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if ok == 0 {
		return storage.Space{}, fmt.Errorf("s3: scratch capacity: %w", callErr)
	}
	return storage.Space{Total: total, Free: available}, nil
}
