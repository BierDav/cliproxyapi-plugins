# Source provenance

This is an independent project. There is no automatic upstream synchronization
or plugin release dependency.

The C-ABI scaffold derives from Arthur Sommer's MIT-licensed Copilot plugin;
that copyright and license are retained in the repository's NOTICE. The Make
OAuth, MCP, gateway, prompt and executor code is new.

Make endpoints, OAuth scopes and agent module parameters were checked against
Make's MCP server (`https://mcp.make.com/v2`), its OAuth metadata and the
`ai-local-agent:RunLocalAIAgent` module specification.

CLIProxyAPI is a separate host and a pinned v8.0.48 SDK dependency, not forked here.
