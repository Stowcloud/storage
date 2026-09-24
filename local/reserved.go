//go:build linux

package local

import "strings"

var reservedPrefixes = []string{".sctrash", ".scpart-", ".scmeta", ".scindex"}

func IsReservedName(name string) bool {
	for _, prefix := range reservedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
