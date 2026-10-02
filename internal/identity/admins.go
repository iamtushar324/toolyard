package identity

import "context"

// AdminEmails lists the emails of the active admins, oldest first: the
// people who can sign a shared server in, named to a member whose agent
// hit such a server. Admins without an email on file are left out.
func (s *Service) AdminEmails(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT email FROM users
        WHERE role = ? AND status = ? AND email IS NOT NULL AND email != ''
        ORDER BY created_at, id`, RoleAdmin, StatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
