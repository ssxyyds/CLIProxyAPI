# Codex Enhancements

This branch keeps ssxyyds Codex-specific behavior in a thin enhancement layer so upstream syncs stay manageable. Prefer adding new `codex_*` files and small hook points over broad edits to shared routing, config, or executor code.

## Routing

Quota-aware Codex selection is the primary routing mode for this branch. The example configuration defaults to:

```yaml
routing:
  strategy: codex-quota-score
```

If `routing.strategy` is omitted entirely, CPA still normalizes the empty value to upstream's historical `round-robin` fallback. Keep the explicit `codex-quota-score` setting in branch configs when Codex quota-aware scheduling is expected.

`codex-quota-score` ranks Codex OAuth-like accounts by live quota state. When the candidate set is not fully Codex OAuth-like, it falls back to fill-first behavior.

The live score is:

```text
quota_remaining / max(hours_until_quota_reset, 1) + expiry_urgency_bonus + manual_adjustment
```

Important fields:

- `quota_remaining`: prefers the weekly window when present, otherwise falls back to the five-hour window.
- `hours_until_quota_reset`: derived from the selected quota window reset timestamp.
- `expiry_urgency_bonus`: small bonus for accounts whose selected quota reset is within 24 hours.
- `manual_adjustment`: operator-controlled score adjustment, accepted range `-100` to `100`.

The selected Codex account is sticky while it remains usable. Sticky state is released before a reset probe so a recovered account can be recalculated cleanly.

Manual score save recalculates the sticky selection immediately. In API contract terms: manual score save recalculates current selection. If an operator raises one account's manual adjustment high enough, the next `/codex-state` read and subsequent routed requests should reflect the new winner without waiting for the 15 minute巡检 loop.

Implementation note: `codex-quota-score` must not be handled by the scheduler fast path. The fast path keeps provider/model ready buckets and cursor caches for the built-in `round-robin` and `fill-first` strategies, but it does not evaluate live Codex quota scores. `CodexQuotaScoreSelector` deliberately uses the normal selector path so real requests call the quota score logic directly. This preserves the scheduler cache behavior for existing strategies while ensuring request logs, usage records, and dashboard current selections all point at the actual score-selected account.

## Management API

All endpoints are under `/v0/management` and require the normal management auth middleware.

### GET /v0/management/codex-state

Returns Codex OAuth-like account state for dashboards and operations pages.

Important response fields:

- `id`, `auth_index`, `name`, `email`
- `provider`, `status`, `disabled`, `unavailable`
- `account_type`, `plan_type`, `id_token.plan_type`: Codex plan/package hints such as `free`, `team`, or `plus`
- `account`: account label derived from the auth metadata
- `on_device`: whether this account is currently sticky-selected
- `codex_quota`: five-hour and weekly quota windows
- `codex_score_explanation`: score inputs, final score, freshness, and disqualifier reason
- `codex_manual_score_adjustment`
- `codex_computed_score`
- `codex_score_reason`
- `codex_last_selection_reason`
- `routing_strategy`: current routing strategy, for example `codex-quota-score`, `fill-first`, or `round-robin`
- `current_selections`: the currently sticky-selected Codex account per model

The response also includes `summary` for pool-level statistics and `current_selections` for model-level routing visibility:

```json
{
  "summary": {
    "accounts": {
      "total": 100,
      "active": 95,
      "cooldown": 5,
      "unavailable": 5,
      "disabled": 0
    },
    "weekly": {
      "known": 92,
      "limit": 9200,
      "remaining": 7310,
      "remaining_ratio": 0.7945
    },
    "five_hour": {
      "known": 88,
      "limit": 3520,
      "remaining": 870,
      "remaining_ratio": 0.2471
    },
    "last_refresh_at": "2026-05-21T06:30:00Z"
  },
  "current_selections": [
    {
      "model": "gpt-5.4",
      "id": "auth-id",
      "auth_index": "codex-auth-1",
      "name": "Codex Primary",
      "email": "user@example.com",
      "account": "user@example.com"
    }
  ],
  "routing_strategy": "codex-quota-score"
}
```

`known` counts accounts that currently have a parsed quota bucket for that window. `last_refresh_at` is the newest quota refresh timestamp among visible Codex OAuth-like accounts.

### PATCH /v0/management/codex-state/manual-score

Adjusts an account's manual score weight.

Request:

```json
{"id":"auth-id","value":3.5}
```

You can also identify the account by `name` or `auth_index`. Values must be finite and between `-100` and `100`.

After a successful save, CPA recalculates the current sticky selection from the latest quota and score state. The response includes `on_device` for the updated account so dashboards can refresh the highlighted winner immediately.

### POST /v0/management/codex-state/refresh

Refreshes one account or all Codex OAuth-like accounts.

Single account:

```json
{"id":"auth-id"}
```

All accounts:

```json
{"all":true}
```

### POST /v0/management/codex-state/recalc

Recalculates the sticky-selected Codex account from current quota state and returns the selected account.

## Quota Refresh

Codex OAuth-like accounts get `refresh_interval_seconds = 900` by default. The auto-refresh loop treats this as a 15 minute account巡检 cadence.

Refresh behavior:

- Refresh OAuth tokens when a refresh token exists.
- Fetch Codex quota from `/usage`.
- Parse five-hour and weekly windows.
- If `/usage` cannot provide the weekly window, send the configured low-cost bootstrap ping so new accounts can trigger upstream quota statistics.
- Preserve previous window data if the upstream response omits one window.
- Apply quota cooldown when `blocked_until` is present.
- If a reset window is reached, send a recovery probe request to refresh the next five-hour cycle. This reset recovery probe takes priority over bootstrap probing.

When validating real Codex accounts from the local CPA development environment, use the local HTTP proxy on port 7899 for ChatGPT upstream traffic:

```yaml
proxy-url: "http://127.0.0.1:7899"
```

The quota refresh client honors credential `proxy_url` first, then global `proxy-url`. Direct access to `https://chatgpt.com/backend-api/codex/usage` may time out in this environment, so real-account巡检 verification should use the 7899 proxy unless a credential explicitly sets another working proxy.

## Probe

Default probe:

```yaml
codex-quota-probe:
  model: "gpt-5.4-mini"
  prompt: "ping"
```

The probe calls `/responses/compact` and requires usage evidence in the successful response. The implementation does not record probe成本 or probe次数.

The same probe payload is also used as a bootstrap refresh for new Codex accounts when `/usage` is missing the weekly window. Weekly quota is the scheduling-critical window for `codex-quota-score`; if weekly is already known but five-hour is missing, CPA keeps the partial state and waits for normal usage, headers, or later巡检 to fill five-hour data instead of spending tokens.

Bootstrap probing is intentionally conservative:

- A successful bootstrap ping marks the account `bootstrap_status=pending`; CPA does not immediately issue a second `/usage` request.
- Later巡检 observes whether upstream has populated weekly data.
- Once weekly data is known, bootstrap is marked `complete`.
- Repeated bootstrap attempts use backoff: `15m`, then `1h`, then `6h`, then `24h`.
- Recovery probes around quota reset windows keep their original priority and are not delayed by bootstrap state.

## Quota Refresh Gate

The quota refresh gate protects large account pools.

Hard-coded defaults:

- quota refresh gate concurrency: `2`
- minimum interval between Codex refresh/probe starts: `3s`
- stable account jitter window: `2m`

The scheduler still checks accounts every 15 minutes, but Codex OAuth-like refresh jobs go through a queue. Duplicate jobs for the same auth are ignored while that auth is pending or running. This avoids 100+ accounts probing at the same instant.

## Files

Codex-specific implementation files:

- `sdk/cliproxy/auth/codex_state.go`
- `sdk/cliproxy/auth/codex_score_explanation.go`
- `sdk/cliproxy/auth/codex_refresh_gate.go`
- `sdk/cliproxy/routing_selector.go`
- `internal/api/handlers/management/codex_state.go`
- `internal/runtime/executor/codex_quota_refresh.go`
- `internal/runtime/executor/codex_executor_refresh_test.go`
- `internal/config/codex_quota_probe.go`

Shared files should keep only small hook points:

- `internal/api/server.go`: management routes
- `internal/config/config.go`: config field
- `sdk/cliproxy/auth/conductor.go`: refresh/update hooks and the selector path that keeps `codex-quota-score` out of the scheduler fast path
- `internal/runtime/executor/codex_executor.go`: call into quota refresh helpers

## 统计页面 接入建议

Use `GET /v0/management/codex-state` as the stable read API. A dashboard should not read auth files directly.

CPA is the Codex authority. It owns quota refresh/probe, score calculation, current model-account selections, plan type hints, reset times, and routing strategy. The stats dashboard should consume this state instead of duplicating Codex refresh logic.

Recommended panels:

- Overview: current routing strategy, weekly remaining total, and five-hour remaining total
- Credentials/Auth Files: compact per-row Codex score and manual adjustment controls
- Current sticky/on-device account, preferably from `current_selections`
- Pool summary: total limit, remaining quota, known account count, and remaining ratio
- Weekly remaining and reset time
- Five-hour remaining and reset time
- Score explanation and manual adjustment
- Refresh status and probe status
- Cooldown/disqualifier reason

Keep row-level Credentials UI compact. Do not add the CPA patrol refresh timestamp to every row; it is pool-level state and normally updates at the same cadence for many accounts. A separate Codex Pool page can remain as a diagnostic/deprecated component, but it should not be the primary operator entry when Credentials already covers account operations.

Recommended actions:

- Set manual score with `PATCH /codex-state/manual-score`
- Refresh one account with `POST /codex-state/refresh`
- Refresh all accounts with `POST /codex-state/refresh {"all":true}`
- Recalculate current account with `POST /codex-state/recalc`

## Upstream Sync Notes

When syncing upstream:

- Keep Codex enhancements on `codex-enhancement`.
- Prefer resolving conflicts by preserving upstream shared logic and reapplying Codex hooks.
- Add new Codex behavior in `codex_*` files whenever possible.
- Re-run `go test ./...` after every sync.
