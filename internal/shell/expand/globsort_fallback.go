package expand

import "os"

func globStatExtraFallback(fi os.FileInfo, key string) int64 {
	switch key {
	case "atime", "ctime":
		return fi.ModTime().UnixNano()
	default: // "blocks"
		return (fi.Size() + 511) / 512
	}
}
