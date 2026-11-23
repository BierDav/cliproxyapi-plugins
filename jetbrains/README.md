# Native JetBrains provider

This C-ABI plugin runs inside the existing CLIProxyAPI v8.0.48 process. It adds
JetBrains OAuth, credential refresh, per-account model discovery, execution and
normalized quota reporting. There is no additional service or container.

Build with Go 1.26+ and a C compiler:

```sh
make test
make build   # dist/cliproxyapi-jetbrains.so
```

Source, tests and builds are maintained here without automatic upstream plugin
updates. See NOTICE.md for provenance and the repository's NOTICE for the
retained MIT license.

Run `make test` and `make build` on the target architecture. Install the library
at `/data/plugins/linux/<amd64-or-arm64>/cliproxyapi-jetbrains.so` using atomic
rename while the host is stopped. Enable both `plugins.enabled: true` and
`plugins.configs.cliproxyapi-jetbrains.enabled: true` and restart the host.
The provider key remains `jetbrains`, preserving existing sign-ins and routes.
CI tests and builds on native Linux/amd64 and Linux/arm64 runners.

Start a sign-in through the management API with its License ID and submit the
localhost callback URL. The plugin uses the
host's native OAuth session and callback endpoints. Dashboard sign-ins read optional
License IDs from the `license_id` and `license_ids` plugin settings
(`plugins.configs.cliproxyapi-jetbrains`, editable in the plugin
settings form) and fall back to the account's free `grazie-lite` AI license when none is configured.
JetBrains refresh tokens
live in separate `/data/auth/jetbrains-<email>.json` credentials; the host owns
their routing, persistence and scheduled refresh. The OAuth token is saved
before license activation so a wrong License ID does not discard the sign-in.
New accounts are registered with JetBrains' AI service before license activation.
Pending PKCE verifiers are stored with mode `0600` for recovery of a failed
exchange. They expire after ten minutes, are deleted after a successful
exchange, and expired files are removed when another sign-in starts.
If candidate or configured License IDs are initially rejected (for example, non-AI
educational licenses before a payment card is added), sign-in and token refresh
automatically fall back to obtaining and activating the account's free JetBrains AI
license (`grazie-lite`). When credentials refresh, configured licenses take precedence
over fallback licenses and the credential automatically upgrades once eligible or trusted.
Correct an incorrect `license_id` in the credential using the authenticated
credential management API, then refresh the credential.

Explicit `jetbrains/` model IDs keep the existing provider pools separate.
Native Anthropic requests retain thinking signatures and prompt cache fields.
OpenAI supports Chat Completions and Responses; xAI uses Responses, and Gemini
uses JetBrains' aggregate protocol. The
official SDK translates client protocols, and the plugin frames SSE across
arbitrary transport chunk boundaries. The native quota API reports the included
allowance and the monthly refill time from JetBrains' refill metadata endpoint,
without spending or resetting quota.

The C-ABI scaffold and SSE frame decoder derive from Arthur Sommer's Copilot
plugin under the MIT license retained in the repository's NOTICE. Protocol endpoint names and model IDs
were checked against JetBrains' profile API and the public
`RoadToNowhereX/jb-ai-proxy` protocol documentation.

Only the management API can initiate sign-ins or read account quotas. Tokens
and upstream error bodies are not included in plugin error messages. Provider
HTTP 477 maps to 429 so the existing credential scheduler can fail over.
