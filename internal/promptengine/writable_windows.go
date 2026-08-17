package promptengine

import "os"

// writable reports whether the directory can be written to. Windows
// permissions do not reduce to a mode bit, and the honest cheap answer
// is the read-only attribute: anything finer would need a security
// descriptor round trip, which is not a prompt-path cost (#47).
func writable(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return fi.Mode().Perm()&0o200 != 0
}
