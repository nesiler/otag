`recorded.json` contains Codex 0.160.0 payloads recorded on 2026-10-05 in
`spike/spike.db` (the source database is intentionally not committed).
`recorded_event_id` identifies the original row. The workspace and account-home
prefixes are sanitized; event fields and tool inputs are preserved.

`events.json` supplements those recordings with small protocol-shaped cases for
usage, rate limits, pending approvals, and noise, checked against the locally
generated Codex 0.161.0 JSON schemas. No fixture test makes an inference request.
