module toolkit

go 1.26.4

// go1.26.4's stdlib carries GO-2026-5856 (crypto/tls ECH privacy leak),
// reachable via http.Server + the anthropic client; the precommit vuln scan
// flags it. Fixed in go1.26.5; GOTOOLCHAIN=auto downloads it. Continues the
// pin-to-stay-patched intent behind the go1.26.4 directive.
toolchain go1.26.5

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/google/jsonschema-go v0.4.3
	github.com/modelcontextprotocol/go-sdk v1.6.0
	github.com/yalue/onnxruntime_go v1.30.1
	go.yaml.in/yaml/v3 v3.0.4
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/pretty v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

require (
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/telemetry v0.0.0-20260421165255-392afab6f40e // indirect
	golang.org/x/tools v0.44.0 // indirect
	golang.org/x/vuln v1.3.0 // indirect
	modernc.org/sqlite v1.51.0
)

tool golang.org/x/vuln/cmd/govulncheck
