// Package app assembles and runs the independently authenticated Canary HTTP
// and PWA host. It owns the app state directory, its single-process lock,
// device authentication, live view cache, alert delivery integration, and
// optional relay connector. Broker connectivity, trading decisions, and risk
// policy remain behind the typed daemon client and are not reimplemented here.
package app
