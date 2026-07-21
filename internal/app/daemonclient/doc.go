// Package daemonclient adapts the app host to the daemon's typed RPC surface.
// It can carry read, preview, and paired-device action requests, but capability
// to call a method is not authority: the daemon retains policy, preview-token,
// account, mode, freeze, and broker-write enforcement.
package daemonclient
