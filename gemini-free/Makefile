test:
	go test -mod=readonly -race ./... ../framework/...

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -mod=readonly -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o dist/cliproxyapi-gemini-free.so .
