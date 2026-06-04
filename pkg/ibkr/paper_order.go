package ibkr

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const paperOrderMode = "paper"

// PaperOrderGate is the daemon-supplied evidence required by the narrow paper
// write wrappers. It is not a broker permission; TWS/Gateway can still reject.
type PaperOrderGate struct {
	Mode     string
	Account  string
	Endpoint string
	Host     string
	Port     int
	ClientID int
}

func (g PaperOrderGate) validateConnection(c *Connection) error {
	if err := g.validate(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("paper order gate: connection is nil")
	}
	if strings.TrimSpace(c.config.Account) != "" &&
		!strings.EqualFold(strings.TrimSpace(c.config.Account), strings.TrimSpace(g.Account)) {
		return fmt.Errorf("paper order gate account %q does not match connection account %q", g.Account, c.config.Account)
	}
	host := strings.TrimSpace(g.Host)
	if host == "" {
		host = strings.TrimSpace(c.config.Host)
	}
	port := g.Port
	if port == 0 {
		port = c.config.Port
	}
	if strings.TrimSpace(c.config.Host) != "" && !strings.EqualFold(strings.TrimSpace(c.config.Host), host) {
		return fmt.Errorf("paper order gate host %q does not match connection host %q", host, c.config.Host)
	}
	if c.config.Port != 0 && port != c.config.Port {
		return fmt.Errorf("paper order gate port %d does not match connection port %d", port, c.config.Port)
	}
	if c.config.ClientID != 0 && g.ClientID != c.config.ClientID {
		return fmt.Errorf("paper order gate client ID %d does not match connection client ID %d", g.ClientID, c.config.ClientID)
	}
	return nil
}

func (g PaperOrderGate) validate() error {
	if !strings.EqualFold(strings.TrimSpace(g.Mode), paperOrderMode) {
		return fmt.Errorf("paper order gate requires mode=paper")
	}
	account := strings.TrimSpace(g.Account)
	if account == "" {
		return fmt.Errorf("paper order gate requires account")
	}
	if strings.EqualFold(account, "All") {
		return fmt.Errorf("paper order gate rejects aggregate account All")
	}
	host, port := g.endpointParts()
	if host == "" {
		return fmt.Errorf("paper order gate requires endpoint host")
	}
	if port == 0 {
		return fmt.Errorf("paper order gate requires endpoint port")
	}
	if g.ClientID <= 0 {
		return fmt.Errorf("paper order gate requires positive client ID")
	}
	if !paperAccount(account) {
		return fmt.Errorf("paper order gate requires a concrete DU paper account")
	}
	return nil
}

func (g PaperOrderGate) endpointParts() (string, int) {
	host := strings.TrimSpace(g.Host)
	port := g.Port
	endpoint := strings.TrimSpace(g.Endpoint)
	if endpoint == "" {
		return host, port
	}
	if h, p, err := net.SplitHostPort(endpoint); err == nil {
		host = h
		if parsed, parseErr := strconv.Atoi(p); parseErr == nil {
			port = parsed
		}
		return host, port
	}
	if host == "" {
		host = endpoint
	}
	return host, port
}

func paperAccount(account string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(account)), "DU")
}
