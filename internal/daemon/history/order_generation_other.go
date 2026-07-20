//go:build !darwin && !linux

package history

import "os"

type orderChangeIdentity struct{}

func orderChangeIdentityFor(os.FileInfo) (orderChangeIdentity, bool) {
	return orderChangeIdentity{}, false
}
