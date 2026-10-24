// Package store opens the SQLite database and applies schema migrations.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/amwolff/yamp/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store bundles the connection pool with the generated queries.
type Store struct {
	DB *sql.DB
	*db.Queries
}

// Open opens or creates the database and migrates it. Without fsync it uses
// synchronous=NORMAL: consistent after power loss, minus the last transactions.
func Open(ctx context.Context, path string, fsync bool) (*Store, error) {
	sync := "NORMAL"
	if fsync {
		sync = "FULL"
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous("+sync+")")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	conn, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Store{DB: conn, Queries: db.New(conn)}, nil
}

// Close closes the connection pool.
func (s *Store) Close() error { return s.DB.Close() }

// Tx runs fn in a transaction and commits it if fn returns nil.
func (s *Store) Tx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// migrate applies every embedded migration whose number is above PRAGMA
// user_version. Files are named NNNN_description.sql.
func migrate(ctx context.Context, conn *sql.DB) error {
	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	type migration struct {
		version int
		name    string
	}
	var todo []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return fmt.Errorf("migration %q: missing version prefix", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		if v > current {
			todo = append(todo, migration{v, e.Name()})
		}
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].version < todo[j].version })
	for _, m := range todo {
		body, err := migrations.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
