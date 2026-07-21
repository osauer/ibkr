// Package auth manages Canary device pairing and app-session credentials.
// Durable device grants and credential hashes belong to the app state store;
// pairing sessions, challenges, and bearer sessions are process-local,
// time-bounded values. Callers must treat all raw nonces, secrets, signatures,
// cookie values, and session tokens as sensitive untrusted input.
package auth
