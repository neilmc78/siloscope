package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// accountInfo is what an ingester knows about a login on a media server.
type accountInfo struct {
	Source      string
	ExternalID  string
	Username    string
	DisplayName string
	Email       string
	PlexUUID    string
	Thumb       string
}

// ensureAccount upserts an account and returns its id. A new account gets a
// person of its own; autoLink later merges people that share a Plex identity.
func (s *store) ensureAccount(ctx context.Context, info accountInfo, seen time.Time) (int64, error) {
	if info.Source == "" || info.ExternalID == "" {
		return 0, errors.New("account needs a source and external id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	seenAt := any(nil)
	if !seen.IsZero() {
		seenAt = seen.UnixMilli()
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE source = ? AND external_id = ?`, info.Source, info.ExternalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		name := firstNonEmpty(info.DisplayName, info.Username, info.Email, fallbackAccountName(info.Source, info.ExternalID))
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		var personID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO people (display_name, created_at, updated_at) VALUES (?, ?, ?) RETURNING id`, name, now, now).Scan(&personID); err != nil {
			return 0, err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO accounts (source, external_id, username, display_name, email, plex_uuid, thumb_url, person_id, first_seen, last_seen, updated_at)
			VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?) RETURNING id`,
			info.Source, info.ExternalID, info.Username, info.DisplayName, info.Email, strings.ToLower(info.PlexUUID), info.Thumb, personID, seenAt, seenAt, now).Scan(&id)
		if err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE accounts SET
			username = COALESCE(NULLIF(?, ''), username),
			display_name = COALESCE(NULLIF(?, ''), display_name),
			email = COALESCE(NULLIF(?, ''), email),
			plex_uuid = COALESCE(NULLIF(?, ''), plex_uuid),
			thumb_url = COALESCE(NULLIF(?, ''), thumb_url),
			first_seen = COALESCE(MIN(first_seen, ?), first_seen, ?),
			last_seen = COALESCE(MAX(last_seen, ?), last_seen, ?),
			updated_at = ?
		WHERE id = ?`,
		info.Username, info.DisplayName, info.Email, strings.ToLower(info.PlexUUID), info.Thumb, seenAt, seenAt, seenAt, seenAt, now, id)
	return id, err
}

// autoLink puts Silo and Plex accounts that share a plex.tv identity under one
// person. Accounts an administrator linked or unlinked by hand are left alone.
func (s *store) autoLink(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT plex_uuid, GROUP_CONCAT(id) FROM accounts
		WHERE plex_uuid IS NOT NULL AND link_method = 'auto'
		GROUP BY plex_uuid HAVING COUNT(DISTINCT person_id) > 1`)
	if err != nil {
		return err
	}
	type group struct{ ids []string }
	var groups []group
	for rows.Next() {
		var uuid, ids string
		if err := rows.Scan(&uuid, &ids); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, group{strings.Split(ids, ",")})
	}
	rows.Close()
	for _, g := range groups {
		if err := s.mergeAccountsLocked(ctx, g.ids, "auto"); err != nil {
			return err
		}
	}
	return s.refreshPersonNamesLocked(ctx)
}

// refreshPersonNamesLocked names each person after its best-named account:
// a Plex display name first (usually the friendliest), then a Silo username,
// then anything. People an administrator renamed keep their name.
func (s *store) refreshPersonNamesLocked(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE people SET display_name = COALESCE((
			SELECT COALESCE(NULLIF(a.display_name, ''), NULLIF(a.username, ''), NULLIF(a.email, ''))
			FROM accounts a WHERE a.person_id = people.id
			  AND COALESCE(NULLIF(a.display_name, ''), NULLIF(a.username, ''), NULLIF(a.email, '')) IS NOT NULL
			ORDER BY a.source = 'plex' DESC, a.id LIMIT 1), display_name), updated_at = ?
		WHERE name_manual = 0`, time.Now().UnixMilli())
	return err
}

func fallbackAccountName(source, externalID string) string {
	if source == "plex" {
		return "Plex user " + externalID
	}
	return "Silo user " + externalID
}

// mergeAccountsLocked moves the accounts under the person of the first Plex
// account among them (else the first account) and removes emptied people.
func (s *store) mergeAccountsLocked(ctx context.Context, accountIDs []string, method string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(accountIDs)), ", ")
	args := make([]any, len(accountIDs))
	for index, id := range accountIDs {
		args[index] = id
	}
	var target int64
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM accounts WHERE id IN (`+placeholders+`)
		ORDER BY source = 'plex' DESC, id LIMIT 1`, args...).Scan(&target); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET person_id = ?, link_method = ?, updated_at = ? WHERE id IN (`+placeholders+`)`,
		append([]any{target, method, time.Now().UnixMilli()}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM people WHERE id NOT IN (SELECT person_id FROM accounts)`); err != nil {
		return err
	}
	return tx.Commit()
}

type personView struct {
	ID          int64         `json:"id"`
	DisplayName string        `json:"display_name"`
	Accounts    []accountView `json:"accounts"`
}

type accountView struct {
	ID          int64  `json:"id"`
	Source      string `json:"source"`
	ExternalID  string `json:"external_id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	PlexLinked  bool   `json:"plex_identity"`
	LinkMethod  string `json:"link_method"`
	LastSeen    *int64 `json:"last_seen,omitempty"`
	Plays       int64  `json:"plays"`
}

func (s *store) listPeople(ctx context.Context) ([]personView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.display_name, a.id, a.source, a.external_id, COALESCE(a.username, ''),
			COALESCE(a.display_name, ''), COALESCE(a.email, ''), a.plex_uuid IS NOT NULL, a.link_method, a.last_seen,
			(SELECT COUNT(*) FROM plays WHERE account_id = a.id)
		FROM people p JOIN accounts a ON a.person_id = p.id
		ORDER BY p.display_name COLLATE NOCASE, p.id, a.source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	people := []personView{}
	for rows.Next() {
		var personID int64
		var personName string
		var account accountView
		var lastSeen sql.NullInt64
		if err := rows.Scan(&personID, &personName, &account.ID, &account.Source, &account.ExternalID, &account.Username,
			&account.DisplayName, &account.Email, &account.PlexLinked, &account.LinkMethod, &lastSeen, &account.Plays); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			account.LastSeen = &lastSeen.Int64
		}
		if len(people) == 0 || people[len(people)-1].ID != personID {
			people = append(people, personView{ID: personID, DisplayName: personName})
		}
		people[len(people)-1].Accounts = append(people[len(people)-1].Accounts, account)
	}
	return people, rows.Err()
}

// unlinkAccount gives the account a person of its own and pins it so autoLink
// does not merge it back.
func (s *store) unlinkAccount(ctx context.Context, accountID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(display_name, ''), NULLIF(username, ''), NULLIF(email, ''), CASE source WHEN 'plex' THEN 'Plex user ' ELSE 'Silo user ' END || external_id) FROM accounts WHERE id = ?`, accountID).Scan(&name); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	var personID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO people (display_name, created_at, updated_at) VALUES (?, ?, ?) RETURNING id`, name, now, now).Scan(&personID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET person_id = ?, link_method = 'manual', updated_at = ? WHERE id = ?`, personID, now, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM people WHERE id NOT IN (SELECT person_id FROM accounts)`); err != nil {
		return err
	}
	return tx.Commit()
}

// peopleJSON serves GET /api/people and POST /api/people with an action:
// {"action":"link","account_ids":[1,2]}, {"action":"unlink","account_id":1},
// {"action":"rename","person_id":1,"display_name":"Sam"}.
func (app *application) peopleJSON(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if app.store == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "History storage is not configured")
		return
	}
	ctx := request.Context()
	if request.Method == http.MethodPost {
		var input struct {
			Action      string  `json:"action"`
			AccountIDs  []int64 `json:"account_ids"`
			AccountID   int64   `json:"account_id"`
			PersonID    int64   `json:"person_id"`
			DisplayName string  `json:"display_name"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil {
			writeJSONError(writer, http.StatusBadRequest, "Invalid request")
			return
		}
		var err error
		switch input.Action {
		case "link":
			if len(input.AccountIDs) < 2 || len(input.AccountIDs) > 20 {
				writeJSONError(writer, http.StatusBadRequest, "Choose at least two accounts")
				return
			}
			ids := make([]string, len(input.AccountIDs))
			for index, id := range input.AccountIDs {
				ids[index] = jsonInt(id)
			}
			app.store.mu.Lock()
			err = app.store.mergeAccountsLocked(ctx, ids, "manual")
			app.store.mu.Unlock()
		case "unlink":
			err = app.store.unlinkAccount(ctx, input.AccountID)
		case "rename":
			name := strings.TrimSpace(input.DisplayName)
			if name == "" || len([]rune(name)) > 100 {
				writeJSONError(writer, http.StatusBadRequest, "Name must be 1 to 100 characters")
				return
			}
			_, err = app.store.db.ExecContext(ctx, `UPDATE people SET display_name = ?, name_manual = 1, updated_at = ? WHERE id = ?`, name, time.Now().UnixMilli(), input.PersonID)
		default:
			writeJSONError(writer, http.StatusBadRequest, "Unknown action")
			return
		}
		if err != nil {
			writeJSONError(writer, http.StatusBadRequest, "The change could not be saved")
			return
		}
	}
	people, err := app.store.listPeople(ctx)
	if err != nil {
		writeJSONError(writer, http.StatusInternalServerError, "People could not be read")
		return
	}
	_ = json.NewEncoder(writer).Encode(people)
}

func jsonInt(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
