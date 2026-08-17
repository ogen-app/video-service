# Shared gRPC contract (CON-220). video.v1 lives in buf.build/ogen-app/proto;
# this repo generates the server stubs from a pinned version of that module.
# Bump PROTO_VERSION to adopt a new contract, then `make proto` and commit gen/.
PROTO_MODULE  = buf.build/ogen-app/proto
PROTO_VERSION = v1.0.0

.PHONY: proto
proto:
	buf generate $(PROTO_MODULE):$(PROTO_VERSION) --path video/v1/video.proto
