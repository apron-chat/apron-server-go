module github.com/apron-chat/apron-server-go

go 1.27.0

toolchain go1.27.4

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/apron-chat/apron-server-go/apron v0.0.0-20261127214907-a76e8d09ddac
	github.com/coder/websocket v1.8.15
	github.com/fxamacker/cbor/v2 v2.10.0
	github.com/go-jose/go-jose/v4 v4.1.9
	github.com/go-webauthn/webauthn v0.18.2
	github.com/jessevdk/go-flags v1.6.1
	golang.org/x/crypto v0.59.0
	golang.org/x/image v0.48.0
	golang.org/x/net v0.63.0
	golang.org/x/oauth2 v0.38.0
	golang.org/x/sync v0.25.0
	golang.org/x/text v0.44.0
	golang.org/x/time v0.16.0
	modernc.org/sqlite v1.63.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.4.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/philhofer/fwd v1.2.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tinylib/msgp v1.6.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	honnef.co/go/tools v0.8.2 // indirect
	modernc.org/libc v1.82.14 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.13.1 // indirect
)

tool honnef.co/go/tools/cmd/staticcheck
