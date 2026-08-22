module github.com/cairnobs/cairnobs/hack/demo-hosts-fixture

go 1.25.0

replace github.com/cairnobs/cairnobs/proto => ../../proto

require (
	github.com/cairnobs/cairnobs/proto v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.83.0
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
