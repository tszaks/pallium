//go:build darwin

package codeindex

import (
	"os"
	"syscall"
)

// SF_DATALESS is the macOS stat flag for a file whose bytes are not local.
const sfDataless = 0x40000000

func isDataless(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Flags&sfDataless != 0
}
