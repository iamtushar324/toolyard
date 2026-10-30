---
name: rollback-release
description: "Roll back a bad production release to the last good version and verify that errors return to normal."
metadata:
  triggers: "a deploy broke production; error rates jumped after a release; users report failures right after shipping"
  tags: "ops, incident, release"
---
# Rollback release

Roll back a bad production release to the last good version and verify that errors return to normal.

## Steps

1. Read the request and confirm the scope.
2. Follow the checklist for this kind of work.
3. Report what changed and how it was verified.
