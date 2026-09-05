# Security Review and Remediation

Review date: **2026-09-06**. Historical vulnerable baseline: `1e10c565dc5443e5bafcc391a95563df51c08bad`. Shared-login baseline: `ad50edd2cb2754f28efb14486e5b2dcc3bc41753`. The user subsequently authorized PUB-01 through PUB-05 remediation, dependency repairs and upgrade validation.

**Verification status:** the first full remediation run at `30e14c1` passed application, race, database, browser and historical-upgrade checks, but both final-image vulnerability gates failed. System-package remediation and an all-green matching-SHA run remain pending. All execution is on remote GitHub Actions; local work is editing, static review, Git/task bookkeeping and evidence retrieval. No production deployment or penetration test has occurred.

## Findings and Disposition

Severity is engineering triage, not proof of production exploitation. Public reachability does not imply anonymous authorization.

| Finding | Historical prerequisites and impact | Remediation |
| --- | --- | --- |
| **PUB-01 / High**: compatibility search bypassed provider restrictions | A valid restricted search Token could use excluded providers through explicit selection or global defaults, receiving unauthorized results or incurring cost. Scope/admission limits still applied; this was not admin access or a raw-Key leak. | All three compatibility handlers apply native-equivalent Token provider policy before cache/orchestration. Mounted tests assert excluded-provider/key/cache call counts, default/explicit selections, attribution and rejection accounting. |
| **PUB-02 / Medium**, higher if settings contain usable secrets: global provider configuration | Ordinary Tokens, or anonymous callers with business auth off, could read internal URLs/settings and disabled providers. Embedded credentials would leak, but no deployed secret was inspected. | Delete `GET /v1/providers`; retain full configuration only at protected `/api/admin/providers`. No new public projection API. |
| **PUB-03 / Low**, visibility-dependent: global usage summary | Ordinary Tokens or auth-off callers could infer instance activity from totals, not queries or Token identities. | Delete `GET /v1/usage/summary`; retain `/api/admin/usage/summary`. Cover auth-on/off, missing/business/admin credentials and unchanged admin reads. |
| **PUB-04 / Medium**: anonymous MCP discovery amplification | Repeated schema-list entries amplified response construction despite the default 1 MiB input cap. Outage/peak-memory impact was not measured. | Maximum 32 batch requests; discovery response budget 256 KiB, tool-bearing response budget 16 MiB including framing. Incremental batch encoding, cancellation checks and bounded errors without oversized ID echo. |
| **PUB-05 / Medium**: retained login-attempt state | Unauthenticated distinct usernames accumulated map entries without global bounds or unrelated-entry expiry, independently of IP spoofing. | Username maximum 256 bytes; at most 4096 username/IP windows reserved before lookup. Lazily prune the bounded map, preserve active lockouts and reject new entries at capacity with 429. No extra service/background cleaner. |
| **AUTH-01 / Historical**: forged client-IP bucket selection | Unconditional forwarding-header trust let callers vary login-limit attribution; passwords were still required. | Trust only a real loopback peer's single valid X-Real-IP; Nginx overwrites/removes forwarded client fields. Go and real HTTP/HTTPS regressions passed at `ad50edd` and remain in CI. |

Source: [handlers](../backend/internal/api/handlers.go), [login limiter](../backend/internal/api/auth.go), [MCP](../backend/internal/api/mcp.go). Focused fixtures: [route policy](../backend/internal/api/security_routes_test.go), [login limits](../backend/internal/api/login_limits_test.go), existing auth/MCP suites and packaged browser tests. Source presence alone does not close executable acceptance.

Login capacity rejection is fail-closed: active locks are never evicted to admit arbitrary names. Saturation may temporarily reject new login buckets until expiry; valid sessions and admin Keys are not revoked. These local controls are not distributed rate limits or a universal DoS guarantee.

## Public Interface Inventory

Nginx serves the SPA and forwards `/api/`, `/v1/`, `/healthz` and MCP aliases. Reachability is operator-controlled. Authorities: [Handler.Mount](../backend/internal/api/handlers.go), [NewServer](../backend/internal/api/server.go), [Nginx](../deploy/nginx.conf).

| Method/path | Authorization and disclosure |
| --- | --- |
| `GET /healthz` | Anonymous 200/503 status; bounded DB ping, no configuration dump. |
| `GET`/`HEAD` SPA HTML/JS/CSS/images and frontend route fallback | Public assets; protected data comes from separately authenticated APIs. No debug/pprof/Swagger mount. |
| `POST /api/admin/login` | Retained username/password entry; JSON, trusted supplied Origin and browser proof. Raw HTTP clients can also use it; the proof header is not a secret/password substitute. |
| `POST /v1/search` | Ordinary Token `search` scope and provider restrictions, or valid admin Key. |
| `POST /v1/extract` | `extract` scope, Extract-capable allowed providers and URL/input/network policy. |
| `POST /v1/compat/tavily/search` | Tavily toggle; search authorization and provider restrictions. |
| `POST /v1/compat/tavily/extract` | Tavily toggle; Extract authorization and private-target policy. |
| `POST /v1/compat/serper/search` | Serper toggle; search authorization and provider restrictions. |
| `POST /v1/compat/openai/responses-search` | OpenAI toggle; search authorization and provider restrictions. |
| `GET /v1/providers`, `GET /v1/usage/summary` | Removed; 404 even with admin Key. Use the protected management equivalents. |

Business auth is the persisted `RuntimeSettings.APIAuthRequired`, default true. Auth-off skips retained business REST/MCP credential/scope/provider/quota admission, never management auth. The legacy `API_AUTH_REQUIRED` environment field is not applied to stored settings in the reviewed composition: an `.env` value alone is not evidence of the active policy. Read authenticated runtime settings.

Ordinary `osr_` Tokens must be enabled, valid and within configured admission limits. Empty provider allowlists are unrestricted; restrictive lists also control omitted-provider defaults. Admin `oak_` Keys intentionally bypass ordinary-Token admission, not provider/key operational controls. Password Cookies and old `adm_` headers never authorize business REST/MCP.

Bearer takes precedence over X-API-Key; selected invalid credentials do not borrow another credential. Only the two Tavily routes accept top-level JSON `api_key` when no nonempty header credential was selected. There is no business Cookie/query-credential fallback.

### MCP

When enabled, MCP is mounted at the configured path and `/v1/mcp`. Anonymous discovery is intentional and bounded; it never authorizes paid tool calls.

| Method | Boundary |
| --- | --- |
| `GET` at each alias | Public protocol/tool/endpoint metadata. SSE Accept gives 405; no stream/session allocation. |
| `DELETE` | 405; no session-deletion operation. |
| `OPTIONS` | Public preflight, not authentication or tool execution. |
| `POST initialize`, `ping`, `tools/list`, `resources/list`, `resources/templates/list`, `prompts/list` | Anonymous discovery, bounded output. |
| `notifications/initialized` without ID | Ignored, 202. Notifications do not execute search/extraction. |
| `tools/call` | Business Token/Key gate unless effective business auth is off; scope/provider policy for the actual tool. No Cookie auth. |
| Unknown request methods | Existing auth policy, then method-not-found; no arbitrary dispatch. |
| JSON-RPC array | 1-32 requests, at most one `tools/call`; required auth before execution. Discovery-only arrays use the smaller output budget. |
| Malformed/empty JSON or invalid array size | Bounded validation error, no upstream work. Oversized encoded replies give 413; cancellation gives 408. |

### Protected Management

Paths below are relative to `/api/admin`, requiring a live Cookie plus browser proof or a valid admin-Key header. Ordinary Tokens are rejected even with `*` scope. All management responses are no-store.

| Methods/paths | Operation |
| --- | --- |
| `GET /me`; `POST /logout` | Session probe; revoke captured Cookie session only. Key logout does not revoke an incidental Cookie. |
| `GET /dashboard`, `/metrics`, `/audit-logs` | Global management telemetry/audit. |
| `GET /providers`, `/providers/health`; `PATCH /providers/{name}` | Full provider configuration/health and update. |
| `GET /keys`; `POST /keys`; `PATCH`/`DELETE /keys/{id}` | Provider-Key lifecycle. |
| `GET /keys/{id}/secret`; `POST /keys/{id}/test`, `/keys/{id}/quota` | Explicit secret reveal or upstream test/quota work. |
| `GET /tokens`; `POST /tokens`; `PATCH`/`DELETE /tokens/{id}` | Business-Token lifecycle/scopes/providers/limits. |
| `GET /tokens/{id}/secret` | Explicit Token reveal. |
| `GET`/`PUT /settings` | Runtime policy. |
| `GET`/`POST /settings/admin-api-key` | Key metadata or first creation/rotation. |
| `GET /logs`, `/logs/{id}` | Request/provider-call detail. |
| `GET /usage/summary`, `/usage/billing` | Global management totals/billing. |
| `POST /playground/search` | Admin search. |

No anonymous Key bootstrap. Existing password login with Cookie jar/proof, or the authenticated UI, creates the first Key. See [admin API contract](./admin-api-key.md).

## Dependency Remediation

[Initial Actions evidence](https://github.com/vihor3/searchmeld/actions/runs/33990094730) at `8c0076a` resolved npm's summary into esbuild (low), nanoid (high), postcss (high). It was not proof of two exploitable production services. Go reachability found x/text, pgx/v4 and pgproto3/v2 issues; two lacked fixes in the v4 chain.

[Candidate generation](https://github.com/vihor3/searchmeld/actions/runs/33990495387) at `400aaef` produced reviewed manifests remotely. Full npm audit returned zero vulnerabilities and govulncheck zero affected vulnerabilities. This preliminary run mechanically converted pgx imports on the runner; it does not replace testing the actual migration. No local resolver/installation ran.

| Dependency | Selected repair |
| --- | --- |
| Go 1.22 | Supported compiler1.26.8, matching CI; module minimum1.26.0 required by updated dependencies. |
| Alpine 3.20 runtime | Supported3.22.5, retaining explicit `postgresql16` packages. Builders use supported Alpine3.23. |
| Node/installation | Node22.23.2; image and CI require committed lockfiles through `npm ci`. |
| Vite/esbuild | 7.3.6 / 0.28.2; no Vite-major or Vue/UI-library upgrade. |
| nanoid/postcss | 3.3.18 / 8.5.28. |
| Go libraries | pgx/v5 5.9.2 removes obsolete v4/protocol modules; chi5.3.2, x/crypto0.56.0, x/text0.41.0 and required x/sync0.22.0. Preserve startup Ping, parameterized SQL and persisted credentials. |

Official sources: [Go policy](https://go.dev/doc/devel/release#policy), [Alpine support](https://alpinelinux.org/releases/), [Vite7.3.6](https://github.com/vitejs/vite/blob/v7.3.6/packages/vite/CHANGELOG.md), [pgx5.9.2](https://github.com/jackc/pgx/blob/v5.9.2/CHANGELOG.md). esbuild's [Windows-development-server advisory](https://github.com/evanw/esbuild/security/advisories/GHSA-g7r4-m6w7-qqqr) does not describe the final Linux runtime. Do not attribute npm counts to the withdrawn Deno advisory.

Permanent [CI](../.github/workflows/ci.yml) audits committed npm dependencies including build packages, Go call reachability, and both final images' OS/Go packages. npm/image gates start at low severity; no blanket ignore-unfixed or audit-error suppression. Reports retain seven days. The temporary candidate workflow is deleted after import. Chunk-size warnings are performance notices, not vulnerabilities.

The actual-source Go report at `30e14c1` has zero affected symbols and zero imported-package vulnerabilities. It separately lists module-only [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), which affects the unimported OpenPGP packages in x/crypto, not the password-hashing package this project uses. This is an applicability distinction, not a scanner suppression or a claim that every package in the dependency module is safe.

## Upgrade and Auth Evidence

Shared-login [run33988333263](https://github.com/vihor3/searchmeld/actions/runs/33988333263) passed five jobs at `ad50edd`: Go/PostgreSQL, frontend/mock browser, shell, external-image build and all-in-one real HTTP/HTTPS. This corrected the first-run restart-fixture and mobile-layout failures.

Remediation adds [historical DB upgrade](../deploy/database_upgrade_test.cjs) and extends [real browser integration](../deploy/admin_cookie_integration_test.cjs) to external PostgreSQL. Old source is immutable `1e10c565dc5443e5bafcc391a95563df51c08bad`, not an invented release. Fixtures use synthetic accounts/Keys/data, disposable volumes and stable loopback HTTP/browser origins across Docker port changes. External PostgreSQL stays on an internal network without host publication; its application also joins the normal bridge to support loopback-only HTTP publication. Historical upgrade applications use a run-scoped normal bridge with loopback-only HTTP publication; providers are disabled and redirected to container loopback before synthetic provider secrets are created. These application networks are not an egress firewall. Upgrade helpers and PG17 containers use no network. Fixtures must prove credential/data preservation, incompatible PGDATA rejection before mutation, and recovery from a separate copy. No dumps, secrets or traces are artifacts.

[First full remediation run33991999625](https://github.com/vihor3/searchmeld/actions/runs/33991999625), exact SHA `30e14c114d79c5ee22c2302fdc9f0932585a754e`, passed Go formatting/vet/race tests with PostgreSQL, frontend build/session regression, shell regressions, both packaged HTTP/HTTPS browser modes, and the historical upgrade/backup/PG17 mismatch-recovery fixture. Synthetic desktop/mobile PNGs were inspected. Both image builds succeeded but their vulnerability gates failed on system packages; this run does not close overall acceptance. The next image changes must rerun these checks.

## Operational Boundaries

These deployment/product contracts are distinct from the five findings and dependency/upgrade scope; they are not silently fixed or a blanket safety claim:

- Sessions are process-local, default fixed TTL24h; restart revokes them. Independent browser profiles/multiple servers do not share a store. Upgrade reloads old JavaScript and requires one login; old `adm_` scripts move to Cookie-jar/proof or existing `oak_` headers.
- HTTPS termination requires real `ADMIN_PUBLIC_ORIGIN`. Direct HTTP is unencrypted and for trusted local/internal use. Cookies are not port-isolated; untrusted apps must not share the host. Origin/proof is browser CSRF protection, not a password substitute.
- Token quota reads precede later accounting, so concurrent work may overshoot remaining quota. RPM/key reservations are local. Shared search cache is not private per-user storage; no hard distributed spending/isolation guarantee.
- Extract URL/DNS preflight is bounded, but upstream extractors perform target fetching/redirect resolution. Configure their egress boundary; preflight is not DNS pinning or universal SSRF protection. See [Extract deployment boundary](./extract.md).
- Default input cap is1MiB; upstream reads and Extract content/logs are bounded. MCP now adds independent limits; Search fan-out/content still lacks Extract-equivalent global budgets. No load test/general availability certification.
- Encryption/hashing and explicit admin reveal protect stored credentials. Arbitrary queries, URL parameters, settings and upstream errors may contain sensitive content; truncation is not redaction. Protect deployment keys/backups/log access; deployed secret-bearing payloads were not inspected.
- Reviewed SQL uses parameters and Vue renders escaped text, but upstream links/content remain untrusted. Auth tests and scanners are not a whole-product SQL/XSS/security certification.

Update this report when routes, versions, proxy trust, visibility or limits change. Keep source fixes, exact-SHA tested acceptance and deployment status distinct.
