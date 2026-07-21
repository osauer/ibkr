// Package apphttp registers the Canary app's authenticated HTTP routes and
// maps internal state and daemon RPC values onto explicit browser DTOs. It owns
// request validation, redaction, and paired-device attribution, while runtime
// policy and broker-write authority remain daemon-owned.
package apphttp
