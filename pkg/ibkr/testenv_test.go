package ibkr

// setServerVersionReady configures a connection with the given server version
// and marks both the handshake and broker ID namespace ready so unit tests can
// exercise outbound encoding paths without recreating the async startup frames.
func setServerVersionReady(c *Connection, version int) {
	c.serverVersion = version
	c.signalHandshakeReady()
	c.observeNextValidOrderID(1)
}
