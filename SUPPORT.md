# Getting help

This is a personal open-source project. There is no commercial support, and no
response-time guarantee.

| I want to… | Go to |
| --- | --- |
| Understand what this does | [README](README.md) |
| Understand *why* it is built this way | [Decision records](docs/decisions/) |
| Run it locally | [Development Compose](deploy/compose/README.md) |
| Integrate a client | [Clients](docs/CLIENTS.md) and [API v1](docs/API_V1.md) |
| See what is actually verified | [End-to-end suite](tests/e2e/README.md) |
| Report a bug | [Open an issue](https://github.com/mischapogr/platform-ipam/issues) |
| Ask a question or propose an idea | [Discussions](https://github.com/mischapogr/platform-ipam/discussions) |
| Report a vulnerability | [SECURITY.md](SECURITY.md) — not a public issue |

Before opening an issue, please include the output of the relevant
`scripts/ai/check-*` command. Those print compact JSON, and **exit code 2 means
verification was blocked, not that it passed** — that distinction usually
explains the problem on its own.
