//go:build linux || darwin || freebsd

package s3

import (
	"fmt"

	"github.com/stowcloud/storage"
	"golang.org/x/sys/unix"
)

func spaceCapacity(path string) (storage.Space, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return storage.Space{}, fmt.Errorf("s3: stat scratch capacity: %w", err)
	}
	blockSize := uint64(stat.Bsize)
	return storage.Space{Total: uint64(stat.Blocks) * blockSize, Free: uint64(stat.Bavail) * blockSize}, nil
}
