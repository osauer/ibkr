package ibkr

import "context"

// GatewayBootstrapper is an optional hook that can start or ensure the IBKR gateway
// is running before the connector tries to establish a session. Implementations might
// launch a managed gateway process, trigger Docker compose services, or ping a health
// endpoint. Returning nil indicates the bootstrap action completed (or was unnecessary).
// Returning an error allows the connector to log the failure and continue in degraded mode.
type GatewayBootstrapper interface {
	EnsureGateway(ctx context.Context) error
}

// GatewayBootstrapFunc adapts a function so it can be used as a GatewayBootstrapper.
type GatewayBootstrapFunc func(context.Context) error

// EnsureGateway invokes the underlying function if present.
func (f GatewayBootstrapFunc) EnsureGateway(ctx context.Context) error {
	if f == nil {
		return nil
	}
	return f(ctx)
}
