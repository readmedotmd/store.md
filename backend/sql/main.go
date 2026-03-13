package sqlstore

import (
	"context"
	"database/sql"
	"fmt"

	storemd "github.com/readmedotmd/store.md"
)

// PlaceholderStyle controls how query parameter placeholders are generated.
type PlaceholderStyle int

const (
	// QuestionMark uses ? placeholders (SQLite, MySQL).
	QuestionMark PlaceholderStyle = iota
	// DollarSign uses $1, $2, ... placeholders (PostgreSQL).
	DollarSign
)

// Option configures a StoreSQL.
type Option func(*StoreSQL)

// WithPlaceholderStyle sets the placeholder style for SQL queries.
// Defaults to QuestionMark if not specified.
func WithPlaceholderStyle(style PlaceholderStyle) Option {
	return func(s *StoreSQL) {
		s.placeholderStyle = style
	}
}

// StoreSQL implements the storemd.Store interface using database/sql.
type StoreSQL struct {
	db               *sql.DB
	placeholderStyle PlaceholderStyle
}

// New creates a new StoreSQL and initializes the kv_store table.
func New(db *sql.DB, opts ...Option) (*StoreSQL, error) {
	s := &StoreSQL{db: db}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

// ph returns the placeholder for the given 1-based parameter position.
func (s *StoreSQL) ph(n int) string {
	if s.placeholderStyle == DollarSign {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (s *StoreSQL) init() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS kv_store (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
	return err
}

func (s *StoreSQL) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM kv_store WHERE key = "+s.ph(1), key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", storemd.NotFoundError
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *StoreSQL) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO kv_store (key, value) VALUES ("+s.ph(1)+", "+s.ph(2)+") ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

func (s *StoreSQL) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		"INSERT INTO kv_store (key, value) VALUES ("+s.ph(1)+", "+s.ph(2)+") ON CONFLICT (key) DO NOTHING",
		key, value,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *StoreSQL) Delete(ctx context.Context, key string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM kv_store WHERE key = "+s.ph(1), key)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return storemd.NotFoundError
	}
	return nil
}

// Clear removes all key-value pairs from the store.
func (s *StoreSQL) Clear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM kv_store")
	return err
}

func (s *StoreSQL) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	query := "SELECT key, value FROM kv_store"
	var queryArgs []interface{}
	var conditions []string
	paramIdx := 1

	if args.Prefix != "" {
		conditions = append(conditions, "key LIKE "+s.ph(paramIdx))
		paramIdx++
		queryArgs = append(queryArgs, args.Prefix+"%")
	}

	if args.StartAfter != "" {
		conditions = append(conditions, "key > "+s.ph(paramIdx))
		paramIdx++
		queryArgs = append(queryArgs, args.StartAfter)
	}

	for i, cond := range conditions {
		if i == 0 {
			query += " WHERE " + cond
		} else {
			query += " AND " + cond
		}
	}

	query += " ORDER BY key ASC"

	if args.Limit > 0 {
		query += " LIMIT " + s.ph(paramIdx)
		paramIdx++
		queryArgs = append(queryArgs, args.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []storemd.KeyValuePair
	for rows.Next() {
		var kv storemd.KeyValuePair
		if err := rows.Scan(&kv.Key, &kv.Value); err != nil {
			return nil, err
		}
		result = append(result, kv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if result == nil {
		result = []storemd.KeyValuePair{}
	}

	return result, nil
}
