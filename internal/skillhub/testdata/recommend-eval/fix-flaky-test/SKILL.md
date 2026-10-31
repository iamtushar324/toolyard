---
name: fix-flaky-test
description: "Diagnose and fix a flaky test that passes and fails at random by finding the race, the shared state or the timing assumption."
metadata:
  when-to-use: "a test fails only sometimes; a retry makes the build green"
  tags: "testing, ci"
---
# Fix flaky test

Diagnose and fix a flaky test that passes and fails at random by finding the race, the shared state or the timing assumption.

## Steps

1. Read the request and confirm the scope.
2. Follow the checklist for this kind of work.
3. Report what changed and how it was verified.
