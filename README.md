# CLIProxyAPI plugins

Native C-ABI provider plugins for the official
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8.0.48 host. The
repository is one Go module; each plugin has its own directory and README, and
all of them are built on the shared framework:

| Directory | Library | Provider |
| --- | --- | --- |
| [`copilot/`](copilot) | `cliproxyapi-copilot.so` | GitHub Copilot |
| [`jetbrains/`](jetbrains) | `cliproxyapi-jetbrains.so` | JetBrains AI |
| [`make/`](make) | `cliproxyapi-make.so` | Make AI Agents |
| [`gemini-free/`](gemini-free) | `cliproxyapi-gemini-free.so` | Gemini Developer API Free tier |
| [`framework/`](framework) | | shared plugin runtime |

## Framework

`framework` holds everything that is the same for every provider plugin:

- `framework.Plugin` describes a plugin (identifier, metadata, capabilities,
  configuration, quiesce and shutdown hooks) and routes RPC methods to the
  `Handler`s it registers. It answers `plugin.register`/`plugin.reconfigure`,
  the `*.identifier` methods and unknown methods itself, and turns errors into
  envelopes: an `*framework.Error` keeps its own fields, an error with a
  `StatusCode()` gets that status (retryable for 429 and 5xx), and anything
  else is a 500.
- `framework.Host` makes the host callbacks: upstream HTTP (`Do`), upstream
  streams (`OpenStream`, `ReadStream`, `CloseStream`), the client's output
  stream (`Emit`, `CloseOutput`) and `SaveAuth`. Tests give it a fake `Caller`.
- `framework/abi` is the only cgo code: it exports `cliproxy_plugin_init` and
  serves the `Plugin` a plugin passes to `abi.Serve` from its `init` function.
- `framework/sse` splits upstream server-sent events into frames, and
  `framework.ResolveAuthDir` finds the host's auth directory.

A new plugin is a `package main` directory that builds a `framework.Plugin`,
calls `abi.Serve` from `init` and has an empty `main`.

## Build

Go 1.26.10 and a C compiler are required. Build on the target Linux
architecture:

```sh
make test            # all plugins and the framework
make -C make test    # one plugin and the framework
make -C make build   # one plugin, into make/dist/
```

CI runs the same in the pinned golang image from the `Dockerfile`:

```sh
docker build --build-arg PLUGIN=make .
```

## Deployment

GitHub Actions tests and builds every plugin on native Linux/amd64 and
Linux/arm64. Deploy a plugin by pinning a verified commit and the SHA-256 of
its source archive, then building the plugin's package from the repository
root, since every plugin needs the root `go.mod` and `framework/`:

```sh
go build -mod=readonly -trimpath -buildmode=c-shared \
  -o cliproxyapi-make.so ./make
```

The package paths are `./copilot/cmd/cliproxyapi-copilot`, `./jetbrains`,
`./make` and `./gemini-free`. Install the library as
`<plugins dir>/linux/<amd64-or-arm64>/cliproxyapi-<name>.so` and enable it in
the host's `plugins.configs`, as each plugin's README describes.

When a push to `main` passes, the `deploy` job can notify a deployment: set the
repository secrets `PLUGIN_DEPLOY_URL` and `PLUGIN_DEPLOY_TOKEN` and it POSTs
`{"ref":"main"}` to that URL. Without them the job does nothing. A `[skip ci]`
commit runs no CI, so it should not be deployed.

## License

Apache License 2.0, see [LICENSE](LICENSE). Parts derive from MIT-licensed
code; [NOTICE](NOTICE) retains that copyright and license.
