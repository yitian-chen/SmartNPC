package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

// benchDBPrefix names every database the bench creates, so leftovers from a
// crashed run are easy to find and can never collide with agenttown_dev/stable.
const benchDBPrefix = "memory_bench_"

// benchStoreFactory opens one isolated Store per scenario run. The cleanup
// function releases (and, for MySQL, drops) it; the name is recorded in the
// report so a kept database can be inspected afterwards.
type benchStoreFactory func(ctx context.Context, job int) (store storage.Store, name string, cleanup func(), err error)

// benchMemoryStoreFactory is the database-free fallback (--store memory).
func benchMemoryStoreFactory(context.Context, int) (storage.Store, string, func(), error) {
	return newBenchStore(), "", func() {}, nil
}

// newBenchMySQLStoreFactory gives each scenario run its own temporary database
// on the server named by dsn (any database name in dsn is ignored). Each
// database is opened through production storage.NewMySQLStore, so the schema
// comes from the production embedded migrations and every read/write goes
// through the production SQL. Isolation is per database: concurrent workers and
// repeated runs never share rows.
func newBenchMySQLStoreFactory(ctx context.Context, dsn string, keep bool) (benchStoreFactory, func(), error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	cfg.ParseTime = true
	adminCfg := cfg.Clone()
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return nil, nil, fmt.Errorf("open mysql: %w", err)
	}
	if err = admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		return nil, nil, fmt.Errorf("mysql unreachable (start it with start-dev.sh or pass --store memory): %w", err)
	}
	runID := fmt.Sprintf("%s_%d", time.Now().Format("20060102_150405"), os.Getpid())
	factory := func(ctx context.Context, job int) (storage.Store, string, func(), error) {
		name := fmt.Sprintf("%s%s_%d", benchDBPrefix, runID, job)
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			return nil, "", nil, fmt.Errorf("create database %s: %w", name, err)
		}
		drop := func() {
			if !keep {
				_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
			}
		}
		c := cfg.Clone()
		c.DBName = name
		st, err := storage.NewMySQLStore(ctx, c.FormatDSN())
		if err != nil {
			drop()
			return nil, "", nil, err
		}
		return st, name, func() { _ = st.Close(); drop() }, nil
	}
	return factory, func() { _ = admin.Close() }, nil
}
