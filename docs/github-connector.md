# Per-user GitHub access

Install **GitHub** from Servers, then open **My connections**.
An admin selects **Register GitHub App** once.
GitHub asks the admin to name and create the App.
Toolyard saves the OAuth client secret in its encrypted store.
Toolyard does not keep the App private key or a shared GitHub token.

Install the App on the repositories you select.
Each user then selects **Connect** in My connections.
Members must have access to the GitHub server in Users.
GitHub limits access to the permissions of both the App and the user.

The App requests Metadata read, Contents read, and Pull requests write.
The connector provides these tools:

| Tool | Purpose |
| --- | --- |
| `get_pull_request` | Read a pull request. |
| `list_pull_request_files` | Read changed files and their patches. |
| `list_pull_request_comments` | Read conversation comments. |
| `list_pull_request_reviews` | Read reviews. |
| `list_review_comments` | Read inline comments. |
| `create_pull_request_comment` | Propose a conversation comment. |
| `submit_pull_request_review` | Propose a COMMENT or REQUEST_CHANGES review. |
| `create_review_comment` | Propose an inline comment. |

Each write creates an owner-only request in **Approvals**.
The request shows the repository, pull request, exact text, GitHub account, and commit.
Only the account owner can allow or deny it.
Toolyard sends an allowed write and records the result.
Agents collect the result with `tools.poll_approval` or `tools.wait_for_approval`.
Explicit allow rules, declared read intent, auto rules, and inbox grants cannot bypass this request.
Approving GitHub reviews remain human-only.

A changed commit, changed connection, expired request, or revoked access stops the write.
A hidden operation marker prevents duplicate posts after a process restart.
The connector refuses a write if it cannot exclude a duplicate.
Personal requests do not reach instance-wide chat channels.
Other users cannot see personal requests, results, or GitHub audit events.

## Test

Use a disposable pull request on a selected repository.
Ask an agent to propose a comment.
Confirm that GitHub has no new comment before you allow the request.
Allow it in Approvals.
Confirm that GitHub has one comment under your account.
Propose another comment and deny it.
Confirm that GitHub has no second comment.
Repeat with a second user and confirm that each user sees only their own requests.

The setup uses the [GitHub App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).
