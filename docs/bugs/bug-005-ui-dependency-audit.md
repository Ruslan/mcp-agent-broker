# BUG-005: locked admin UI dependencies fail `npm audit`

- **Status:** Open
- **Priority:** P2
- **Area:** `ui/package-lock.json`, admin UI supply chain
- **Found:** 2026-08-19

## Problem

On 2026-08-19, `npm ci` / `npm audit` reported four vulnerable packages in the
locked dependency tree:

| Package | Direct | Reported severity | Current lock |
|---------|--------|-------------------|--------------|
| `dompurify` | yes | moderate | 3.4.1 |
| `vite` | yes, development | high | 8.0.10 |
| `nanoid` | transitive | high | 3.3.11 |
| `postcss` | transitive | high | 8.5.12 |

The exact advisory set can change as the audit database is updated. DOMPurify is
the most relevant runtime dependency because the admin UI uses it before placing
task, result, and prompt Markdown into `{@html ...}`. The current code passes a
string to the default sanitizer and does not deliberately use the specialized
options named by several advisories, so exploitability must be assessed rather
than inferred from severity alone.

## Impact

The build fails a clean dependency-security audit. A future sanitizer bypass
could expose operators viewing agent-controlled Markdown in the admin UI.

## Suggested fix

Update the lockfile to patched versions using a normal dependency update, review
the resulting major/minor changes, and run the UI build plus a Markdown sanitizing
smoke test. Avoid treating `npm audit fix --force` as the verification step.

## Acceptance criteria

- `npm audit` reports no known vulnerabilities, or every remaining finding has a
  documented applicability decision.
- `npm ci && npm run build` succeeds on a supported Node version.
- Malicious task/result Markdown is covered by an admin UI sanitization test.

