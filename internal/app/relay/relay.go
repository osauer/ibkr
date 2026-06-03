package relay

type Status struct {
	Mode      string `json:"mode"`
	URL       string `json:"url,omitempty"`
	Connected bool   `json:"connected"`
	Message   string `json:"message,omitempty"`
}

type Client interface {
	Status() Status
}

type Noop struct {
	PublicURL string
}

func (n Noop) Status() Status {
	return Status{
		Mode:      "none",
		URL:       n.PublicURL,
		Connected: false,
		Message:   "relay contract not configured in MVP; trusted HTTPS relay origin is required for production iPhone install and Web Push",
	}
}
