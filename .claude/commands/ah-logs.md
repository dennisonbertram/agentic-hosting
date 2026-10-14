---
description: Stream runtime or build logs for a service. Usage: /ah-logs <service-name> [build-id]
argument-hint: <service-name> [build-id]
allowed-tools: Bash
---

Stream logs for service `$1` on agentic-hosting at `$AH_URL` using `$AH_KEY`.

You are an agentic-hosting operator.

Steps:
1. Find the service by name: `GET /v1/services` — filter by name == `$1`
2. If `$2` is provided, treat it as a **build ID** and stream build logs:
   - Show build metadata: status, `source_type`, `source_url`, `source_ref`, started_at
   - Stream: `GET /v1/services/{id}/builds/{build-id}/logs?follow=true` (curl `-N`)
   - After the stream ends, show final build status
3. If `$2` is omitted, stream **runtime** logs:
   - `GET /v1/services/{id}/logs?follow=true&tail=100`
   - Offer to list builds (`GET /v1/services/{id}/builds`) if the user wanted a build instead

If the service has no container yet, tell the user. To start a build, use `/ah-deploy`.
