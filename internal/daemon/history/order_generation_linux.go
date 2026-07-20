//go:build linux

package history

import (
	"os"
	"syscall"
)

type orderChangeIdentity struct {
	seconds     int64
	nanoseconds int64
}

func orderChangeIdentityFor(info os.FileInfo) (orderChangeIdentity, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return orderChangeIdentity{}, false
	}
	return orderChangeIdentity{seconds: st.Ctim.Sec, nanoseconds: st.Ctim.Nsec}, true
}
