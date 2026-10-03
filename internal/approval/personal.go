package approval

// PersonalGitHubField is reserved approval context authored by the gateway.
const PersonalGitHubField = "_toolyard_github"

// PersonalOwner identifies the only person who may inspect or decide this
// request. Ordinary legacy approvals retain their existing access behavior.
func (r *Request) PersonalOwner() string {
	if r == nil {
		return ""
	}
	snapshot, _ := r.Arguments[PersonalGitHubField].(map[string]any)
	owner, _ := snapshot["owner_user_id"].(string)
	return owner
}
