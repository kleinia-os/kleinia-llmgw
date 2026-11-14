package store

import (
	"fmt"
)

// Source is one upstream aggregation entry as managed via llmgwctl.
type Source struct {
	ID       int64
	Name     string
	Type     string // "openai" | "anthropic"
	BaseURL  string
	APIKey   string
	Priority int
	Weight   int
	Enabled  bool
}

// ModelMapping maps a client-facing alias to the source's upstream model.
type ModelMapping struct {
	Alias         string
	UpstreamModel string
}

// AddSource inserts src with its model mappings in one transaction and
// returns the new source ID.
func (s *Store) AddSource(src Source, models []ModelMapping) (int64, error) {
	now := nowRFC3339()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: add source: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`
		INSERT INTO sources (name, type, base_url, api_key, priority, weight, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.Name, src.Type, src.BaseURL, src.APIKey, src.Priority, src.Weight, src.Enabled, now, now)
	if err != nil {
		return 0, fmt.Errorf("store: add source: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: add source: %w", err)
	}
	for _, m := range models {
		if _, err := tx.Exec(`INSERT INTO models (source_id, alias, upstream_model) VALUES (?, ?, ?)`,
			id, m.Alias, m.UpstreamModel); err != nil {
			return 0, fmt.Errorf("store: add source %q model %q: %w", src.Name, m.Alias, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: add source: %w", err)
	}
	return id, nil
}

// ListSources returns all sources, highest priority first.
func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`
		SELECT id, name, type, base_url, api_key, priority, weight, enabled
		FROM sources ORDER BY priority DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Name, &src.Type, &src.BaseURL, &src.APIKey,
			&src.Priority, &src.Weight, &src.Enabled); err != nil {
			return nil, fmt.Errorf("store: list sources: %w", err)
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// UpdateSource updates the source row identified by src.ID and replaces its
// model mappings wholesale.
func (s *Store) UpdateSource(src Source, models []ModelMapping) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: update source %d: %w", src.ID, err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`
		UPDATE sources
		SET name = ?, type = ?, base_url = ?, api_key = ?, priority = ?, weight = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		src.Name, src.Type, src.BaseURL, src.APIKey, src.Priority, src.Weight, src.Enabled,
		nowRFC3339(), src.ID)
	if err != nil {
		return fmt.Errorf("store: update source %d: %w", src.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: update source %d: not found", src.ID)
	}
	if _, err := tx.Exec(`DELETE FROM models WHERE source_id = ?`, src.ID); err != nil {
		return fmt.Errorf("store: update source %d: %w", src.ID, err)
	}
	for _, m := range models {
		if _, err := tx.Exec(`INSERT INTO models (source_id, alias, upstream_model) VALUES (?, ?, ?)`,
			src.ID, m.Alias, m.UpstreamModel); err != nil {
			return fmt.Errorf("store: update source %d model %q: %w", src.ID, m.Alias, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update source %d: %w", src.ID, err)
	}
	return nil
}

// DeleteSource removes a source; its model mappings cascade away with it.
func (s *Store) DeleteSource(id int64) error {
	res, err := s.db.Exec(`DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete source %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: delete source %d: not found", id)
	}
	return nil
}

// SourceModels returns the alias→upstream model mappings of one source,
// ordered by alias.
func (s *Store) SourceModels(id int64) ([]ModelMapping, error) {
	rows, err := s.db.Query(`
		SELECT alias, upstream_model FROM models WHERE source_id = ? ORDER BY alias`, id)
	if err != nil {
		return nil, fmt.Errorf("store: source models %d: %w", id, err)
	}
	defer rows.Close()
	var out []ModelMapping
	for rows.Next() {
		var m ModelMapping
		if err := rows.Scan(&m.Alias, &m.UpstreamModel); err != nil {
			return nil, fmt.Errorf("store: source models %d: %w", id, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
