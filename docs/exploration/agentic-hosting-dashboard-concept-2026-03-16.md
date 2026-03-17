# Agentic Hosting Dashboard Concept

Date: 2026-03-16

## Positioning

`agentic-hosting` is explicitly agent-first and API-first. A dashboard should not become the primary operating surface. It should exist as a supervisory control plane for humans:

- See platform health at a glance
- Understand what agents changed
- Intervene when automation gets stuck
- Audit risky operations without dropping to raw API calls

The product should still feel like "agents are the operators, humans are the supervisors."

## Design Principles

1. Show exceptions before inventory.
2. Prioritize state transitions, failures, and queue pressure over vanity metrics.
3. Every UI action should map cleanly to an existing API call.
4. Preserve machine-readable detail instead of hiding it behind marketing UI.
5. Default to safe read-only views for secrets, destructive actions, and tenant-wide changes.

## Primary Jobs To Be Done

1. Confirm the host is healthy enough for agents to keep deploying.
2. Find which service, build, or database is failing right now.
3. Recover from common failure states quickly:
   - circuit breaker open
   - failed deploy
   - disk pressure
   - stale or failed database provisioning
4. Understand tenant-level blast radius:
   - how many services and databases exist
   - which API keys are active
   - which resources have recent errors
5. Review agent activity without needing shell access.

## Recommended Information Architecture

### 1. Overview

This should be the default landing page.

Top band:
- Platform status
- Docker status
- gVisor status
- Disk used percent
- Active incidents count

Middle band:
- Services by status: running, deploying, stopped, failed, circuit open
- Builds by status: pending, running, failed, succeeded
- Databases by status: provisioning, running, failed
- Queue pressure: deploy queue, build queue

Bottom band:
- Incident feed
- Recent deployments
- Recent database provisioning events
- Tenants with the most active failures

This page should answer: "Is the system healthy, and if not, where do I look next?"

### 2. Services

A dense table, not a card grid.

Columns:
- service name
- tenant
- status
- public URL
- image
- crash count
- circuit state
- last error
- updated at

Fast actions:
- start
- stop
- restart
- reset circuit breaker
- view env
- view builds

The table should support filters for:
- failed
- circuit open
- recently changed
- tenant

### 3. Service Detail

This is the most important drill-down page.

Sections:
- current status and public URL
- deployment history
- build log stream
- last error and crash timeline
- env var management
- linked databases
- container/runtime metadata

The mental model should be "one place to diagnose why this app is not serving traffic."

### 4. Builds

Builds deserve a dedicated queue-oriented view because async build/deploy is one of the core workflows.

Must show:
- build status
- repo URL
- branch/ref
- image tag
- created/started/finished timestamps
- log streaming
- cancel action

Useful derived states:
- stuck pending
- unusually long build
- repeated failures on same service

### 5. Databases

This page should optimize for confidence and safety.

List view:
- name
- tenant
- type
- status
- host/port
- created at

Detail view:
- connection string reveal flow
- credential copy flow
- attached services
- delete confirmation with explicit blast radius

### 6. Tenants

Tenants are the real isolation boundary, so they need a first-class page even if the product is not "team collaboration" yet.

Per-tenant summary:
- status
- service count vs quota
- database count vs quota
- API key count
- recent failures
- recent deploy/build activity

Actions:
- suspend/delete tenant
- rotate or revoke API keys

### 7. Incidents / Activity

The system already has strong operational states but weak human observability around them.

A dedicated feed should normalize events like:
- service created
- deploy started
- deploy failed
- circuit opened
- circuit reset
- database provisioned
- tenant suspended
- API key created/revoked

This is the missing bridge between "agents do everything" and "humans need to trust what happened."

## Recommended Navigation

Primary nav:
- Overview
- Services
- Builds
- Databases
- Tenants
- Activity

Contextual right rail on detail views:
- recent actions
- linked resources
- dangerous actions

## Best First Version

If building an MVP dashboard, do not start with full CRUD.

Start with:
1. Read-only overview
2. Services table
3. Service detail with build logs
4. Databases list
5. Safe recovery actions:
   - restart service
   - reset circuit breaker
   - cancel build

This would already cover the highest-value human supervision tasks.

## API/Data Gaps Exposed By A Dashboard

A good dashboard will immediately want data that the current API does not expose cleanly enough.

Likely missing or weak areas:
- unified activity/events feed
- build queue depth and deploy queue depth
- service-to-database relationship model
- richer health payload for reconciler/GC status
- audit log of who or what triggered an action
- metering/billing visibility from `ah-metering.db`
- service runtime metrics such as restart frequency over time

The dashboard should not paper over these gaps in frontend code. It should drive cleaner API additions.

## UX Notes Specific To Agentic Hosting

- Favor terminal-inspired density over consumer SaaS spaciousness.
- Keep raw identifiers visible: service IDs, build IDs, tenant IDs.
- Show the exact API equivalent for important actions when useful.
- Treat secret reveal as a deliberate, logged action.
- Make "why is automation blocked?" the central question of the interface.

## Suggested Next Step

Build a wireframe around three screens:

1. Overview
2. Services list
3. Service detail

Those three screens cover most of the operator value and will clarify what new backend endpoints are worth adding before any serious frontend build starts.
