# Make provider

This C-ABI plugin runs inside the existing CLIProxyAPI v8.0.48 process. It turns
Make's AI Agent into a normal LLM completion API for Claude Messages, OpenAI
Chat Completions and OpenAI Responses clients. It adds Make OAuth, credential
refresh, gateway provisioning, execution and normalized quota reporting. There is
no additional service, container or webhook.

Build with Go 1.26+ and a C compiler:

```sh
make test
make build   # dist/cliproxyapi-make.so
```

Install the library at `/data/plugins/linux/<amd64-or-arm64>/cliproxyapi-make.so`
using atomic rename while the host is stopped. Enable both `plugins.enabled: true`
and `plugins.configs.cliproxyapi-make.enabled: true`, then restart the host. The
provider key is `make`.

## Sign-in

Start a sign-in from the CLIProxyAPI credentials page (or
`GET /v0/management/make-auth-url`). The plugin registers an OAuth client with
Make's dynamic client registration for MCP, so no Make app or API key has to be
created by hand. Make redirects the browser back to the host's
`/v0/management/oauth-callback`, and the sign-in completes on its own.

After the token exchange the credential is saved first. Then the plugin creates
one on-demand scenario, "CLIProxyAPI gateway (managed, do not edit)", in the
organization's first team, using the built-in "Make's AI Provider" connection.
The scenario, connection and team IDs are stored in the credential. A missing or
outdated gateway is re-created on the next refresh or request.

The signed-in user's email address is stored as `email`, which the auth-file
overview shows and which labels the credential ("Make <organization>" without
one). Sign-in requests the `openid email` scopes, and the address comes from the
ID token in Make's token response. Make's REST API refuses tokens from the MCP
sign-in, so for credentials signed in before these scopes were requested, the
plugin tries `GET /api/v2/users/me`, the OpenID `userinfo` endpoint and the MCP
server at each refresh. Those fail today, so such credentials need one new
sign-in to show an address. A failed lookup is logged with each source's answer,
and sign-in or refresh goes on.

Make access tokens last five minutes. Refresh tokens last six months and rotate on
every refresh, so each rotation is written back to the credential at once. Only the
refresh token is persisted; access tokens stay in memory. Pending PKCE verifiers are
stored with mode `0600`, expire after ten minutes and are deleted after a successful
exchange; expired ones are removed when another sign-in starts.

## Configuration

`plugins.configs.cliproxyapi-make`:

| Field | Default | Meaning |
| --- | --- | --- |
| `public_base_url` | host address | Public CLIProxyAPI origin (for example `https://ai.example.com`). Make then redirects sign-ins to this host and not to the local listener. |
| `team_id` | first team | Make team that hosts the gateway scenario. |
| `claude_thinking` | `false` | Show the model's reasoning to Claude Messages clients as thinking blocks. These blocks carry no Anthropic signature, so a thread that contains them cannot later continue on a real Claude model. OpenAI clients always receive reasoning as `reasoning_content`. |

## How a request runs

Make has no completion endpoint, so every request becomes one run of the gateway
scenario through Make's MCP server (`scenario_run`, then polling
`scenario_execution_get` for long runs, up to 30 minutes). The run is stateless:

- The system prompt holds a fixed protocol description, the client's tool schemas
  and the client's system instructions. It is byte-identical across turns.
- The input message is the whole conversation rendered as an append-only transcript.

Consecutive turns therefore share a long prefix, and Make's provider serves it from
the prompt cache (`cache_read_input_tokens` / `cached_tokens` in the response usage).
Make's agent memory, compaction and history are turned off.

Client tools are not registered in Make. The model requests them as
`<tool_call name="…">{json}</tool_call>` text, and the plugin turns those blocks into
native `tool_use` blocks or `tool_calls` for the client. A block that does not
decode would reach the client as plain text and end its agent turn, so decoding is
tolerant where the intent is unambiguous:

- A `functions.` or `functions__` prefix on a tool name (OpenAI's own tool
  namespace) is removed, and `<tool_call>{"name": …, "arguments": …}</tool_call>`
  is accepted.
- The JSON object is delimited by its own structure, so arguments may contain
  `</tool_call>`.
- Invalid escapes such as `\$` (escaped for a JavaScript template or a shell
  command but not for JSON) are kept as literal backslashes, raw line breaks in
  strings are escaped, and trailing commas are removed.

Anything else, such as an unknown tool name or an unescaped quote, is sent back to
the model: the run is repeated with the rejected reply and the exact error appended
to the transcript, up to two times, reusing the cached prompt prefix. If the reply
still does not decode, it is delivered with a note saying which tool calls were not
run. Blocks quoted in Markdown code are prose and are left alone.

Streaming responses are produced from the finished run:
Claude clients receive `ping` events while the run is in progress.

Models are exposed as `make/<model>`, for example `make/gpt-6.1-sol`. Thinking
budgets and `reasoning_effort` map to the nearest reasoning level the model supports.
Make's AI Provider offers OpenAI models only.

Small models (for example `make/gpt-6-luna`) sometimes call Make's built-in agent tools
instead of following the text protocol. Such runs end with HTTP 422 and do not put
the credential into cooldown. `make/gpt-6.1-sol` follows the protocol reliably.

## Quota

The quota API reports the organization's credits used this period, the plan
allowance plus purchased extra credits, and the next reset time.

## Errors

Tokens and upstream bodies are not included in plugin errors. Make's own error
text is passed on. Rate limits and exhausted credits map to 429; Make data errors
for a run map to 400 or 422; transport and other run failures map to 502.

A paused organization or team (its operations or data-transfer limit is used up)
maps to 429. A streamed request learns about the pause only after its headers
are sent, so for ten minutes afterwards requests on that credential fail with
the 429 before running. A refresh token or OAuth client that Make no longer
knows maps to 401 with a request to sign in to Make again.

Token and client registration requests close their connection afterwards. Make
rotates the refresh token on every use, and a gateway replica that is handing
over cannot store a new one; without a pooled connection, such a replica fails
before Make rotates anything.

See NOTICE.md for provenance and the repository's NOTICE for the retained MIT
license.
