---
name: incident-triage
description: Triage a production incident for a toolyard deployment, find the failing component and decide who to page. Use when health checks fail or users report errors.
---

# Incident triage

1. Check `/v1/health` and `/v1/readyz`.
2. Read the last 15 minutes of logs.
3. If users are affected, follow the nested `page-oncall` skill.
