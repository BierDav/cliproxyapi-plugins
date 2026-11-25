PLUGINS := jetbrains make gemini-free

.PHONY: test build

test:
	go test -mod=readonly -race ./...

build:
	@set -e; for plugin in $(PLUGINS); do $(MAKE) -C $$plugin $@; done
