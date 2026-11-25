# Gemini Free provider

CLIProxyAPI enrolls a Google account and provisions a project for the Gemini
Developer API. It creates or reuses one project for that Google account and
OAuth client, checks that Cloud Billing is unlinked, and creates a service
account bound API key restricted to Gemini. It runs a small generation request
before saving the key. Unless Vertex AI models are configured, Google Cloud
OAuth tokens are used only during enrollment and are kept in memory.

## Gateway setup

Build with `make -C gemini-free build` on the gateway's architecture and
install `dist/cliproxyapi-gemini-free.so` into the CLIProxyAPI plugin directory.

In Google Cloud, set up one OAuth client of type **Web application** for the
gateway. Register the exact redirect URI
`https://ai.example.com/v0/management/oauth-callback` (substitute your own
gateway origin if different). For a private gateway using an external app in
Testing, add each enrolling account as a test user. The app requests
`openid`, `email` and `https://www.googleapis.com/auth/cloud-platform`.
Enable the Cloud Resource Manager, Cloud Billing, Service Usage, IAM and API Keys
APIs in the OAuth client's project. Each enrolling account needs permission to
create and manage its own project.

Set `CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET` in the gateway process
environment. Configure the client ID and origin in CLIProxyAPI:

```yaml
plugins:
  enabled: true
  configs:
    cliproxyapi-gemini-free:
      enabled: true
      oauth_client_id: YOUR_CLIENT_ID.apps.googleusercontent.com
      public_base_url: https://ai.example.com
      pool: gcp
      models: gemini-4-hydrogen
      vertex_models: gemini-4-neon
      vertex_credit: "269.43"
```

`models` is a comma-separated list of Gemini models that every enrolled
account serves. It defaults to `gemini-3.1-flash-lite`. Only models the Free
tier allows are useful here; Google rejects the others with a quota of 0.
`pool` is the shared model prefix and defaults to `gcp`.

`vertex_models` lists models served through Vertex AI, for example with Google
Cloud trial credits. It is empty by default. When it is set, enrollment also
requests offline access and saves the account's Google refresh token. After
the Free tier key is set up, the plugin asks Cloud Billing for the projects
linked to the account's billing accounts and picks one, preferring one where
billing is enabled, then the newest.
It enables the Vertex AI API there and saves the project with the credential.
Requests then call Vertex AI's global endpoint as the signed-in user, so no API
or service account key is needed, which new projects' default organization
policies often forbid. An account whose billing is linked but not yet paid up
is still enrolled; its Vertex AI requests fail until billing works. Accounts
without a billed project serve only the Free tier models. Re-enroll existing
accounts after setting `vertex_models`.

`vertex_credit` is each account's Google Cloud credit in its billing currency,
as the credits page of Cloud Billing shows it (the $300 trial is €269.43 in
EUR accounts). With it set, the plugin's quota reports the remaining credit
and when it ends. Google offers no API for credit balances, so the plugin
derives both:

- **Credit used.** On the first quota read for an account, the plugin enables
  the Pub/Sub, Billing Budget and Cloud Logging APIs in the Vertex AI project.
  It then creates two budgets on the project's billing account, both counting
  from 2 January 2017, the earliest start Cloud Billing accepts, so they cover
  everything since the account opened. `CLIProxy credits` counts cost before
  credits, and `CLIProxy credits after promotions` counts cost after
  promotional credits such as the free trial. The difference is what the
  credit has paid. Cost the credit does not cover, such as Gemini API usage,
  which the trial no longer pays for, is not counted. Both budgets publish to
  the `cliproxy-credit` Pub/Sub topic in the same project. Cloud Billing does
  this a few times a day, so the figure lags real spend by hours. Until both
  budgets have reported, the quota shows the full credit. Budgets created by
  earlier versions, which counted only from the day tracking started, are
  updated in place. The amount of `CLIProxy credits` can be changed in Google
  Cloud, and the next report uses it.
- **End date.** The free trial ends 90 days after it starts, or when its
  credit runs out. The plugin reads the audit logs of the billing account's
  projects for the first time a project was linked to that billing account,
  which happens when the trial starts, and reports the trial's end as the
  quota's reset time. After that date, or once Google closes the billing
  account, the quota shows no credit left. Audit logs are kept for 400 days.
  If the start can't be found, the quota shows no end date.

## Enroll and use an account

On the credentials page, start **Gemini Free** sign-in, then choose or sign
in to the Google account in the browser and authorize the gateway. Repeat
for each account. Keep the credentials page open while the gateway polls
Google Cloud and saves the credential. The management API also supports
`GET /v0/management/gemini-free-auth-url` followed by polling
`GET /v0/management/get-auth-status?state=<state>`.

If Google requires AI Studio terms acceptance or phone verification, complete
the prompt using the enrolled account and start enrollment again. Google does
not provide a public API for this step. Re-enrollment reuses the project and
key if they're already set up.

Every account serves each configured model as `<pool>/<model>`, for example
`gcp/gemini-4-hydrogen`. The gateway pools the accounts behind that ID and
moves to the next account when one hits its quota. The plugin accepts native
Gemini, OpenAI Chat Completions and OpenAI Responses requests.
Enrollment still checks each new key with `gemini-3.1-flash-lite`.
The plugin checks billing at enrollment; keep billing unlinked on the project
to continue using the Free tier.
