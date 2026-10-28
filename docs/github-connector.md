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

## Use another browser profile

Toolyard stops before it opens GitHub.
Select **Continue in this browser** to use the GitHub account of the current browser.
Select **Copy link for another browser** to use the GitHub account of another browser profile.
The link works one time, for 10 minutes.

To register the App from another profile, copy the link from **Register GitHub App**.
Open the link in the other profile and sign in to Toolyard there as an admin.
Select **Continue**, then **Continue to GitHub**.
GitHub creates the App under the GitHub account of that profile and returns to Toolyard there.

To connect your own account from another profile, copy the link from **Connect**.
Open the link in the other profile and select **Continue**.
GitHub does not give Toolyard a verified email.
Thus, sign in to Toolyard as yourself in that profile before you select **Continue**.
Otherwise, Toolyard stores nothing.

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
| `submit_pull_request_review` | Propose a COMMENT, REQUEST_CHANGES, or APPROVE review. |
| `create_review_comment` | Propose an inline comment. |

Each write creates an owner-only request in **Approvals**.
The request shows the repository, pull request, exact text, review event, GitHub account, and commit.
Only the account owner can allow or deny it.
Toolyard sends an allowed write and records the result.
Agents collect the result with `tools.poll_approval` or `tools.wait_for_approval`.
Explicit allow rules, declared read intent, auto rules, and inbox grants cannot bypass this request.
Agents can propose APPROVE reviews through this same flow.
The account owner must explicitly allow each request before Toolyard submits the review.

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
Propose an APPROVE review on a pull request from another author.
Confirm that GitHub has no new review before you allow the request.
Allow it in Approvals and confirm that GitHub records the review for the displayed commit under your account.
Propose another review and change the pull request commit before you allow it.
Confirm that Toolyard refuses the stale request and sends no review.

The setup uses the [GitHub App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).
