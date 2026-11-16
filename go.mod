module github.com/anesthetised/lotsman

go 1.27.1

toolchain go1.27.4

require (
	github.com/amnezia-vpn/amneziawg-go v0.2.20-0.20260724121833-457d920a1a7d
	github.com/google/go-cmp v0.7.0
	github.com/google/nftables v0.3.0
	github.com/mdp/qrterminal/v3 v3.2.1
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
	golang.zx2c4.com/wireguard v0.0.0-20260522210424-ecfc5a8d5446
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.59.0
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/netlink v1.7.3-0.20250113171957-fbb4dce95f42 // indirect
	github.com/mdlayher/socket v0.5.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/telemetry v0.0.0-20261010193011-f71dc92da45e // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/time v0.9.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.9.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c // indirect
	honnef.co/go/tools v0.8.2 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	rsc.io/qr v0.2.0 // indirect
)

tool (
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
