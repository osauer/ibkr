package relay

import "context"

type Status struct {
	Mode      string `json:"mode"`
	URL       string `json:"url,omitempty"`
	Connected bool   `json:"connected"`
	Message   string `json:"message,omitempty"`
}

type Client interface {
	Run(ctx context.Context)
	Status() Status
	PairingURL(raw string) string
}

type Noop struct {
	PublicURL string
}

func (n Noop) Run(context.Context) {}

func (n Noop) Status() Status {
	return Status{
		Mode:      "none",
		URL:       n.PublicURL,
		Connected: false,
		Message:   "relay contract not configured in MVP; trusted HTTPS relay origin is required for production iPhone install and Web Push",
	}
}

func (n Noop) PairingURL(raw string) string {
	return raw
}
