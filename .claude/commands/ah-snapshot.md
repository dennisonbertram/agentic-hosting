---
description: Take a snapshot of a running service for instant environment forking. Usage: /ah-snapshot <service-name> [snapshot-name]
argument-hint: <service-name> [snapshot-name]
allowed-tools: Bash
---

Take a snapshot of service `$1` named `${2:-pre-change-$(date +%Y%m%d)}` on agentic-hosting.

You are an agentic-hosting operator using `$AH_URL` and `$AH_KEY`.

Steps:
1. Find the service by name: `GET /v1/services` — filter by name == `$1`, get its ID. If not found, tell the user.
2. Take the snapshot:
   ```
   POST /v1/services/{id}/snapshots
   Body: {"name": "${2:-pre-change-$(date +%Y%m%d)}"}
   ```
3. Show the snapshot ID, name, and creation time.
4. List all existing snapshots so the user can see the full set: `GET /v1/snapshots`

After taking the snapshot, remind the user:
- Snapshots capture the current image, env config, and resource settings
- They can be used to fork or restore the service in the future
- To delete a snapshot: `DELETE /v1/snapshots/{snapshotID}`
