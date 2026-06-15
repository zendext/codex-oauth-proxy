# Fast Mode Config Design

## Goal

Prevent accidental use of Codex Fast mode, which consumes credits at a higher rate, unless the proxy operator explicitly enables it.

## Behavior

- Add an `allow-fast-mode` boolean config option.
- Default `allow-fast-mode` to `false`.
- When disabled, `/v1/models?client_version=...` removes Fast tier metadata from the embedded Codex model catalog so Codex CLI does not advertise `/fast`.
- When disabled, HTTP requests with Fast service tiers return `400` and are not proxied upstream. Real Codex CLI requests send `service_tier: "priority"` for Fast; compatible clients may also send `"fast"`.
- When disabled, WebSocket `response.create` frames with Fast service tiers are rejected before proxying upstream.
- When enabled, the current model catalog and request forwarding behavior stay unchanged.

## Testing

- Cover model catalog filtering when Fast mode is disabled.
- Cover unchanged model catalog metadata when Fast mode is enabled.
- Cover HTTP request rejection for explicit `service_tier: "fast"` and `"priority"`.
- Cover WebSocket request rejection for explicit Fast service tiers.
- Cover that non-Fast requests still proxy normally.
