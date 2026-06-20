module github.com/MaxDillon/mediabin-golang

go 1.26

require (
	filippo.io/age v1.3.1
	github.com/MaxDillon/daemonizer v0.0.1
	github.com/f4vzvy99f7-sys/vaultblob-go v0.1.12
	golang.org/x/crypto v0.49.0
	golang.org/x/sys v0.42.0
	golang.org/x/term v0.41.0
)

require (
	filippo.io/hpke v0.4.0 // indirect
	github.com/ebitengine/purego v0.10.1 // indirect
)

replace github.com/MaxDillon/daemonizer v0.0.1 => ../../daemonizer
