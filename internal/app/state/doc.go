// Package state owns the Canary app's private durable state, including paired
// devices, push subscriptions, redacted inbox records, attention cursors, and
// app-local delivery evidence. It serializes mutations to state.json; daemon
// runtime and policy state remain separate authorities.
package state
