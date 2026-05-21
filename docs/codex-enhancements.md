# Codex Enhancements

This branch keeps ssxyyds Codex-specific behavior in a thin enhancement layer so upstream syncs stay manageable. Prefer adding new `codex_*` files and small hook points over broad edits to shared routing, config, or executor code.

## Routing

Enable quota-aware Codex selection with:

```yaml
routing:
  strategy: codex-quota-score
```

`codex-quota-score` ranks Codex OAuth-like accounts by live quota state. When the candidate set is not fully Codex OAuth-like, it falls back to fill-first behavior.

The live score is:

```text
weekly_remaining / max(hours_until_weekly_reset, 1) + expiry_urgency_bonus + manual_adjustment
```

Important fields:

- `weekly_remaining`: parsed from Codex `/usage`.
- `hours_until_weekly_reset`: derived from the weekly reset timestamp.
- `expiry_urgency_bonus`: small bonus for accounts whose weekly reset is within 24 hours.
- `manual_adjustment`: operator-controlled weight, accepted range `-100` to `100`.

The selected Codex account is sticky while it remains usable. Sticky state is released before a reset probe so a recovered account can be recalculated cleanly.

## Management API

All endpoints are under `/v0/management` and require the normal management auth middleware.

### GET /v0/management/codex-state

Returns Codex OAuth-like account state for dashboards and operations pages.

Important response fields:

- `id`, `auth_index`, `name`, `email`
- `provider`, `status`, `disabled`, `unavailable`
- `on_device`: whether this account is currently sticky-selected
- `codex_quota`: five-hour and weekly quota windows
- `codex_score_explanation`: score inputs, final score, freshness, and disqualifier reason
- `codex_manual_score_adjustment`
- `codex_computed_score`
- `codex_score_reason`
- `codex_last_selection_reason`

The response also includes `summary` for pool-level statistics:

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
  }
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
- Preserve previous window data if the upstream response omits one window.
- Apply quota cooldown when `blocked_until` is present.
- If a reset window is reached, send a probe request to refresh the next five-hour cycle.

## Probe

Default probe:

```yaml
codex-quota-probe:
  model: "gpt-5.4-mini"
  prompt: "ping"
```

The probe calls `/responses/compact` and requires usage evidence in the successful response. The implementation does not record probe成本 or probe次数.

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
- `sdk/cliproxy/auth/conductor.go`: refresh/update hooks
- `internal/runtime/executor/codex_executor.go`: call into quota refresh helpers

## 统计页面 接入建议

Use `GET /v0/management/codex-state` as the stable read API. A dashboard should not read auth files directly.

Recommended panels:

- Current sticky/on-device account
- Pool summary: total limit, remaining quota, known account count, and remaining ratio
- Weekly remaining and reset time
- Five-hour remaining and reset time
- Score explanation and manual adjustment
- Refresh status and probe status
- Cooldown/disqualifier reason

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
