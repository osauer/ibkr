// Package alerts turns daemon-authored alert state into redacted app inbox
// records and Web Push work. It owns app-side observation, durable-before-send
// dispatch ordering, and presentation; it does not evaluate risk policy or own
// daemon delivery authority.
package alerts
