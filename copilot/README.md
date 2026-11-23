# Copilot plugin

Independent GitHub Copilot provider for the official upstream
CLIProxyAPI v8.0.48 host. This directory owns its source, tests and builds;
it does not fetch or track releases from the original plugin. See NOTICE.md
for provenance and the repository's NOTICE for the retained MIT license.

## Build and install

Go 1.26.10 and a C compiler are required. On the target Linux architecture:

```sh
make test
make build
```

Install `dist/cliproxyapi-copilot.so` at
`/data/plugins/linux/<amd64-or-arm64>/cliproxyapi-copilot.so` using atomic rename
while the host is stopped, then configure:

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  configs:
    cliproxyapi-copilot:
      enabled: true
```

The host's provider key remains `copilot`; existing credential files and model
routes are preserved. GitHub device-code sign-in, token exchange and refresh,
auth-bound model discovery, streaming execution and protocol translation run
inside the host. Native Claude OAuth remains a separate provider. Use the
`excluded_model_prefixes` setting if Copilot models should not overlap it.

## Copilot Fusion and student accounts

The public `/models` catalogue includes models an account cannot select.
Student accounts can run Luna through Fusion even when a direct model request
returns `model_not_supported`. Fusion first calls `/auto` with the latest user
prompt and image flag, then attaches the returned `Copilot-Session-Token` to
inference. These authorized requests work over HTTP/SSE; WebSockets are not
required by the gateway.

The plugin publishes `auto` as **Copilot Fusion**. Account aliases such as
`gh/auto` are supported. GitHub chooses the concrete model and its actual model
name is retained in Responses output. Each request gets its own Fusion session;
session tokens are never stored in credentials or the account token cache.

Explicit model requests that fail with `model_not_supported` retry through
Fusion only when GitHub authorizes that same model. An explicit Sol request
never silently becomes Luna: if Fusion selects a different model, the plugin
returns `model_not_available_for_plan` with the requested and authorized names.
For the tested student accounts, Fusion selects GPT-5.6 Luna; GPT-6.1 Sol's
upstream billing metadata restricts it to Pro+, Business, Enterprise and Max.
Enabling its model policy does not grant access.

This implements Fusion's routing and session authorization, not the official
client's multi-pass cascade or critique orchestration. Direct requests that
already work, including Nano, keep their existing path. Responses, Chat
Completions and Anthropic Messages clients support streaming and
non-streaming execution.

## Model catalogue

Each credential publishes the chat models its `/models` catalogue lists.
Embedding and completion models, and models the account's Copilot policy
disables, are left out. Chat models that name no endpoint, such as
`gpt-4o-mini`, use `/chat/completions`. When GitHub refuses a listed model
for an account with `model_not_supported` (the catalogue also lists models a
plan does not include), the request fails with that error, so the host tries
another credential, and the model leaves that credential's catalogue for 12
hours from its next model refresh.

The management-only `GET /v0/management/copilot-quota?auth_index=<index>` endpoint
reads the selected account's actual included allowance. It neither purchases
overage nor resets credits. Provider errors do not expose credentials.

The plugin also implements CLIProxyAPI's native quota-provider interface. The
Quota Management section can therefore fetch each Copilot account and render
its premium-interaction allowance, remaining fraction, and reset time directly.

CI tests with the race detector and builds native Linux/amd64 and
Linux/arm64 libraries. Deploy from a pinned commit of this repository. A single
host writer owns rotating credentials.
