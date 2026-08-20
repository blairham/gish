package expand

import (
	"os"
	"syscall"
)

// globStatExtra answers the GLOBSORT keys the portable [os.FileInfo]
// surface cannot: access time, inode change time, and block count.
func globStatExtra(fi os.FileInfo, key string) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return globStatExtraFallback(fi, key)
	}
	switch key {
	case "atime":
		return st.Atim.Nano()
	case "ctime":
		return st.Ctim.Nano()
	default: // "blocks"
		return st.Blocks
	}
}
