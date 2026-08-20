//go:build !darwin && !linux

package expand

import "os"

// globStatExtra answers the GLOBSORT keys the portable [os.FileInfo]
// surface cannot. Without a Stat_t to read, approximate: modification
// time stands in for the access and change times, and blocks are
// derived from the size at the historical 512-byte block.
func globStatExtra(fi os.FileInfo, key string) int64 {
	return globStatExtraFallback(fi, key)
}
