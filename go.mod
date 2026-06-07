module github.com/osauer/ibkr

go 1.26.0

toolchain go1.26.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ProtonMail/go-crypto v1.4.1
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/coder/websocket v1.8.14
	github.com/osauer/hyperserve v1.2.0
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/stretchr/testify v1.11.1
	golang.org/x/mod v0.35.0
	golang.org/x/sys v0.43.0
)

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/telemetry v0.0.0-20260421165255-392afab6f40e // indirect
	golang.org/x/time v0.7.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	golang.org/x/tools/gopls v0.21.1 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
