//go:build cgo

package remote

// cgoEnabled records how this binary was built. A cgo build links the
// host libc, which is what breaks "single static binary" on a fleet that
// mixes glibc and musl.
const cgoEnabled = true
