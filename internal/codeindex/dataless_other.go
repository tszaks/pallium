//go:build !darwin

package codeindex

import "os"

func isDataless(os.FileInfo) bool { return false }
