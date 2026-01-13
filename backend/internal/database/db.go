package database

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

var (
	globalDB *sql.DB
	dbMutex  sync.Mutex
)

// InitDB initializes the database connection and runs migrations
func InitDB() (*sql.DB, error) {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/clarity_coder.db"
	}

	// Ensure the directory exists
	dbDir := strings.TrimSuffix(dbPath, "/clarity_coder.db")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// Enable WAL mode - critical for S3 sync safety
	// WAL keeps main .db file always consistent
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	// NORMAL sync is safe with WAL and improves performance
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set synchronous mode: %w", err)
	}

	// Increase busy timeout to handle concurrent access
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		log.Printf("Warning: failed to set busy_timeout: %v", err)
	}

	globalDB = db

	// Run migrations
	if err := runMigrations(db); err != nil {
		db.Close()
		globalDB = nil
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return db, nil
}

// Checkpoint forces WAL to merge into main database file
// Call this before syncing to S3
func Checkpoint() error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if globalDB == nil {
		return nil
	}

	log.Println("Running SQLite WAL checkpoint...")
	_, err := globalDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return fmt.Errorf("WAL checkpoint failed: %w", err)
	}
	log.Println("WAL checkpoint complete")
	return nil
}

// Close performs clean shutdown: checkpoint + close
func Close() error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if globalDB == nil {
		return nil
	}

	// Checkpoint first to merge WAL
	log.Println("Closing database...")
	if _, err := globalDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Printf("Warning: final WAL checkpoint failed: %v", err)
	}

	err := globalDB.Close()
	globalDB = nil
	log.Println("Database closed")
	return err
}

// GetDB returns the global database instance
func GetDB() *sql.DB {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	return globalDB
}

// runMigrations creates the necessary database tables
func runMigrations(db *sql.DB) error {
	migrations := []string{
		// Users table (full schema)
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			email TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			is_active BOOLEAN DEFAULT 1,
			role TEXT NOT NULL DEFAULT 'user'
		)`,
		// API Keys table (full schema)
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			api_key_hash TEXT UNIQUE NOT NULL,
			api_key_prefix TEXT NOT NULL,
			name TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_used_at TIMESTAMP,
			expires_at TIMESTAMP,
			is_active BOOLEAN DEFAULT 1,
			FOREIGN KEY (user_id) REFERENCES users(id)
		)`,
		// Ingestion Jobs table
		`CREATE TABLE IF NOT EXISTS ingestion_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_type TEXT NOT NULL,
			status TEXT NOT NULL,
			progress INTEGER DEFAULT 0,
			total_items INTEGER DEFAULT 0,
			processed_items INTEGER DEFAULT 0,
			error_message TEXT,
			started_at TIMESTAMP,
			completed_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		// Conversations table for chat history
		`CREATE TABLE IF NOT EXISTS conversations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			history TEXT NOT NULL DEFAULT '[]',
			new_message TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id)
		)`,
		// Query Logs table for analytics and debugging
		`CREATE TABLE IF NOT EXISTS query_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			api_key_id INTEGER,
			endpoint TEXT NOT NULL,
			query TEXT NOT NULL,
			response TEXT,
			model_provider TEXT,
			rag_contexts_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			latency_ms INTEGER DEFAULT 0,
			status TEXT NOT NULL,
			error_message TEXT,
			conversation_id INTEGER,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id),
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id),
			FOREIGN KEY (conversation_id) REFERENCES conversations(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_query_logs_user_id ON query_logs(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_query_logs_created_at ON query_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_query_logs_endpoint ON query_logs(endpoint)`,
	}

	for _, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			return err
		}
	}

	// Rename old columns if present.
	renameStatements := []string{
		"ALTER TABLE api_keys RENAME COLUMN key_hash TO api_key_hash",
		"ALTER TABLE api_keys RENAME COLUMN key_prefix TO api_key_prefix",
	}

	for _, stmt := range renameStatements {
		if err := tryExec(db, stmt); err != nil {
			return err
		}
	}

	// Backfill missing columns for existing tables.
	columnAdds := []string{
		"ALTER TABLE users ADD COLUMN email TEXT",
		"ALTER TABLE users ADD COLUMN is_active BOOLEAN DEFAULT 1",
		"ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'",
		"ALTER TABLE api_keys ADD COLUMN api_key_hash TEXT",
		"ALTER TABLE api_keys ADD COLUMN api_key_prefix TEXT",
		"ALTER TABLE api_keys ADD COLUMN name TEXT",
		"ALTER TABLE api_keys ADD COLUMN last_used_at TIMESTAMP",
		"ALTER TABLE api_keys ADD COLUMN expires_at TIMESTAMP",
		"ALTER TABLE api_keys ADD COLUMN is_active BOOLEAN DEFAULT 1",
	}

	for _, stmt := range columnAdds {
		if err := tryExec(db, stmt); err != nil {
			return err
		}
	}

	// Ensure NOT NULL + UNIQUE constraints for api_key_hash on legacy tables.
	if err := ensureUniqueConstraint(db, "api_keys", "api_key_hash"); err != nil {
		return err
	}

	return nil
}

func tryExec(db *sql.DB, statement string) error {
	if _, err := db.Exec(statement); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "duplicate column name") ||
			strings.Contains(msg, "already exists") ||
			strings.Contains(msg, "no such column") {
			return nil
		}
		return err
	}
	return nil
}

func ensureUniqueConstraint(db *sql.DB, table, column string) error {
	query := fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_%s ON %s(%s)`, table, column, table, column)
	_, err := db.Exec(query)
	return err
}
