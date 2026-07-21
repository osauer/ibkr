// Package relay connects the local Canary app host to an optional public relay.
// The relay is transport only: it forwards allowlisted HTTP and streaming paths
// to the local host, while device grants, sessions, authorization, app state,
// daemon access, and broker authority remain local.
package relay
