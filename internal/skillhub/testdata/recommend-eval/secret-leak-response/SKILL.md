---
name: secret-leak-response
description: "Respond to a secret committed to a repository. Revoke it, purge it from the history and check the logs for misuse."
metadata:
  triggers:
    - "a password or token was pushed to a repository"
    - "a scanner reports a leaked credential"
  tags: "security, incident"
---
# Secret leak response

Respond to a secret committed to a repository. Revoke it, purge it from the history and check the logs for misuse.

## Steps

1. Read the request and confirm the scope.
2. Follow the checklist for this kind of work.
3. Report what changed and how it was verified.
