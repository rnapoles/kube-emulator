package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo required
)

// SQLitePersister stores every object as a JSON document, one row per object.
type SQLitePersister struct{ db *sql.DB }

func NewSQLitePersister(path string) (*SQLitePersister, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS objects (
			grp       TEXT    NOT NULL,
			resource  TEXT    NOT NULL,
			namespace TEXT    NOT NULL,
			name      TEXT    NOT NULL,
			rv        INTEGER NOT NULL,
			data      BLOB    NOT NULL,
			PRIMARY KEY (grp, resource, namespace, name)
		)`,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`,
	}
	for _, st := range stmts {
		if _, err := db.Exec(st); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialising sqlite schema: %w", err)
		}
	}
	return &SQLitePersister{db: db}, nil
}

func (p *SQLitePersister) Load() ([]Record, uint64, error) {
	rows, err := p.db.Query(`SELECT grp, resource, namespace, name, rv, data FROM objects`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var recs []Record
	var maxRV uint64
	for rows.Next() {
		var g, r, ns, n string
		var rv int64
		var data []byte
		if err := rows.Scan(&g, &r, &ns, &n, &rv, &data); err != nil {
			return nil, 0, err
		}
		obj := &U{}
		if err := obj.UnmarshalJSON(data); err != nil {
			return nil, 0, fmt.Errorf("decoding %s/%s %s/%s: %w", g, r, ns, n, err)
		}
		recs = append(recs, Record{Key: ResourceKey{Group: g, Resource: r}, Namespace: ns, Name: n, RV: uint64(rv), Obj: obj})
		if uint64(rv) > maxRV {
			maxRV = uint64(rv)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var metaRV int64
	err = p.db.QueryRow(`SELECT v FROM meta WHERE k = 'rv'`).Scan(&metaRV)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	if uint64(metaRV) > maxRV {
		maxRV = uint64(metaRV)
	}
	return recs, maxRV, nil
}

func (p *SQLitePersister) Save(rec Record, rv uint64) error {
	data, err := rec.Obj.MarshalJSON()
	if err != nil {
		return err
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO objects (grp, resource, namespace, name, rv, data) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(grp, resource, namespace, name) DO UPDATE SET rv = excluded.rv, data = excluded.data`,
		rec.Key.Group, rec.Key.Resource, rec.Namespace, rec.Name, int64(rv), data); err != nil {
		return err
	}
	if err := setRV(tx, rv); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *SQLitePersister) Delete(key ResourceKey, ns, name string, rv uint64) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM objects WHERE grp = ? AND resource = ? AND namespace = ? AND name = ?`,
		key.Group, key.Resource, ns, name); err != nil {
		return err
	}
	if err := setRV(tx, rv); err != nil {
		return err
	}
	return tx.Commit()
}

func setRV(tx *sql.Tx, rv uint64) error {
	_, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('rv', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, int64(rv))
	return err
}

func (p *SQLitePersister) Close() error { return p.db.Close() }
