package ibkr

// setServerVersionReady configures a connection with the given server version
// and marks the handshake channel as ready so unit tests can exercise outbound
// encoding paths without blocking on the async handshake lifecycle.
func setServerVersionReady(c *Connection, version int) {
	c.serverVersion = version
	c.signalHandshakeReady()
}
