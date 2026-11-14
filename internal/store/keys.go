package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// Key is one gateway API key with its quota limits. A nil limit field means
// unlimited. AID, when set, binds the key to one kernel agent identity
// (agentd mints such keys at agent creation); Suspended marks a temporary
// hold (agentctl freeze) that keeps the row revocable later.
type Key struct {
	ID          int64
	Name        string
	Key         string // sk-kln-<slug(name)>-<32hex>
	Enabled     bool
	Suspended   bool
	Models      string // comma-separated alias allow-list, or "*"
	RPM         *int64
	TPM         *int64
	DailyTokens *int64
	AID         string // kernel agent id, "" for app keys
	CreatedAt   string
	RevokedAt   string // "" while enabled
}

const keyCols = `id, name, key, enabled, suspended, models, rpm, tpm, daily_tokens, aid, created_at, revoked_at`

// AddKey creates a key for name with a freshly generated secret
// (sk-kln-<slug(name)>-<32hex>, 128 bits from crypto/rand). models is the
// alias allow-list ("*" or a comma-separated list; "" defaults to "*").
func (s *Store) AddKey(name string, models string, rpm, tpm, daily *int64) (Key, error) {
	if models == "" {
		models = "*"
	}
	secret, err := generateKeySecret(name)
	if err != nil {
		return Key{}, fmt.Errorf("store: add key %q: %w", name, err)
	}
	now := nowRFC3339()
	res, err := s.db.Exec(`
		INSERT INTO keys (name, key, enabled, models, rpm, tpm, daily_tokens, created_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		name, secret, models, rpm, tpm, daily, now)
	if err != nil {
		return Key{}, fmt.Errorf("store: add key %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Key{}, fmt.Errorf("store: add key %q: %w", name, err)
	}
	return Key{
		ID:          id,
		Name:        name,
		Key:         secret,
		Enabled:     true,
		Models:      models,
		RPM:         rpm,
		TPM:         tpm,
		DailyTokens: daily,
		CreatedAt:   now,
	}, nil
}

// ListKeys returns all keys in insertion order.
func (s *Store) ListKeys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT ` + keyCols + ` FROM keys ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list keys: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FindKey looks a key up by its secret, enabled and unsuspended keys only
// (the hot path: one indexed equality probe). A missing, revoked or
// suspended key returns an error wrapping sql.ErrNoRows.
func (s *Store) FindKey(key string) (Key, error) {
	row := s.db.QueryRow(`
		SELECT `+keyCols+` FROM keys
		WHERE key = ? AND enabled = 1 AND suspended = 0`, key)
	k, err := scanKey(row)
	if err != nil {
		return Key{}, fmt.Errorf("store: find key: %w", err)
	}
	return k, nil
}

// FindKeyByAID resolves the key bound to a kernel agent identity. Agentfs
// calls arrive with a kernel-attested AID instead of a secret; if agentd
// minted a bound key it carries that agent's limits, otherwise the caller
// falls back to daemon defaults.
func (s *Store) FindKeyByAID(aid string) (Key, error) {
	row := s.db.QueryRow(`
		SELECT `+keyCols+` FROM keys
		WHERE aid = ? AND enabled = 1 AND suspended = 0`, aid)
	k, err := scanKey(row)
	if err != nil {
		return Key{}, fmt.Errorf("store: find key by aid: %w", err)
	}
	return k, nil
}

// BindKeyAID attaches ("" detaches) a kernel agent identity to the key
// named name.
func (s *Store) BindKeyAID(name, aid string) error {
	res, err := s.db.Exec(`UPDATE keys SET aid = ? WHERE name = ?`, aid, name)
	if err != nil {
		return fmt.Errorf("store: bind aid on key %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: bind aid on key %q: not found", name)
	}
	return nil
}

// UpdateKey patches the key named name. nil fields keep the current value;
// a limit pointer ≤ 0 clears the limit (unlimited); models "" or "*" resets
// the allow-list to unrestricted; aid "" detaches the agent binding.
func (s *Store) UpdateKey(name string, models *string, rpm, tpm, daily *int64, aid *string) error {
	sets := make([]string, 0, 5)
	args := make([]any, 0, 6)
	if models != nil {
		m := strings.TrimSpace(*models)
		if m == "" {
			m = "*"
		}
		sets = append(sets, "models = ?")
		args = append(args, m)
	}
	for _, lim := range []struct {
		col  string
		val  *int64
	}{{"rpm", rpm}, {"tpm", tpm}, {"daily_tokens", daily}} {
		if lim.val == nil {
			continue
		}
		if *lim.val > 0 {
			args = append(args, *lim.val)
		} else {
			args = append(args, nil) // 0 → unlimited (NULL)
		}
		sets = append(sets, lim.col+" = ?")
	}
	if aid != nil {
		sets = append(sets, "aid = ?")
		args = append(args, strings.TrimSpace(*aid))
	}
	if len(sets) == 0 {
		return nil // nothing to change; still verify existence below
	}
	args = append(args, name)
	res, err := s.db.Exec(`UPDATE keys SET `+strings.Join(sets, ", ")+
		` WHERE name = ?`, args...)
	if err != nil {
		return fmt.Errorf("store: update key %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: update key %q: not found", name)
	}
	return nil
}

// SetKeySuspended temporarily holds (agentctl freeze) or releases a key
// without touching its revocation state.
func (s *Store) SetKeySuspended(name string, suspended bool) error {
	res, err := s.db.Exec(`UPDATE keys SET suspended = ? WHERE name = ? AND enabled = 1`,
		suspended, name)
	if err != nil {
		return fmt.Errorf("store: suspend key %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: suspend key %q: not found or revoked", name)
	}
	return nil
}

// RevokeKey disables the key named name and stamps revoked_at.
func (s *Store) RevokeKey(name string) error {
	res, err := s.db.Exec(`UPDATE keys SET enabled = 0, revoked_at = ? WHERE name = ?`,
		nowRFC3339(), name)
	if err != nil {
		return fmt.Errorf("store: revoke key %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: revoke key %q: not found", name)
	}
	return nil
}

// scanKey scans one keys row from rows (either *sql.Row or *sql.Rows).
func scanKey(rows interface{ Scan(dest ...any) error }) (Key, error) {
	var k Key
	var rpm, tpm, daily sql.NullInt64
	var revoked sql.NullString
	if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.Enabled, &k.Suspended, &k.Models,
		&rpm, &tpm, &daily, &k.AID, &k.CreatedAt, &revoked); err != nil {
		return Key{}, fmt.Errorf("store: scan key: %w", err)
	}
	k.RPM = nullInt64Ptr(rpm)
	k.TPM = nullInt64Ptr(tpm)
	k.DailyTokens = nullInt64Ptr(daily)
	k.RevokedAt = revoked.String
	return k, nil
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// generateKeySecret mints sk-kln-<slug(name)>-<32hex> with 128 bits of
// entropy from crypto/rand.
func generateKeySecret(name string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return "sk-kln-" + slug(name) + "-" + hex.EncodeToString(buf[:]), nil
}

// slug rewrites name into a URL-safe token: lower case, alphanumerics kept,
// everything else collapsed to single '-'.
func slug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		isAlnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		switch {
		case isAlnum:
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.TrimSuffix(b.String(), "-")
	if s == "" {
		s = "key"
	}
	return s
}
