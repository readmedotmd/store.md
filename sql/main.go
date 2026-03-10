package sqlstore

import (
	"database/sql"

	storemd "github.com/readmedotmd/store.md"
)

// StoreSQL implements the storemd.Store interface using database/sql.
type StoreSQL struct {
	db *sql.DB
}

// New creates a new StoreSQL and initializes the kv_store table.
func New(db *sql.DB) (*StoreSQL, error) {
	s := &StoreSQL{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StoreSQL) init() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS kv_store (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
	return err
}

func (s *StoreSQL) Get(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM kv_store WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", storemd.NotFoundError
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *StoreSQL) Set(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO kv_store (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

func (s *StoreSQL) Delete(key string) error {
	result, err := s.db.Exec("DELETE FROM kv_store WHERE key = ?", key)
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

func (s *StoreSQL) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	query := "SELECT key, value FROM kv_store"
	var queryArgs []interface{}
	var conditions []string

	if args.Prefix != "" {
		conditions = append(conditions, "key LIKE ?")
		queryArgs = append(queryArgs, args.Prefix+"%")
	}

	if args.StartAfter != "" {
		conditions = append(conditions, "key > ?")
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
		query += " LIMIT ?"
		queryArgs = append(queryArgs, args.Limit)
	}

	rows, err := s.db.Query(query, queryArgs...)
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
