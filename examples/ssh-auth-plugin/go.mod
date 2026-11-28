module github.com/nekomeowww/rc/examples/ssh-auth-plugin

go 1.26.0

require (
	github.com/nekomeowww/rc v0.0.0-00010101000000-000000000000
	golang.org/x/crypto v0.58.0
	google.golang.org/grpc v1.85.0
)

require (
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261104224025-d1e00e9f33ab // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/nekomeowww/rc => ../..
