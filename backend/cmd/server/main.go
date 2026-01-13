package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	docs "github.com/Quantum3-Labs/stacks-builder/backend/docs"
	"github.com/Quantum3-Labs/stacks-builder/backend/internal/api"
	"github.com/Quantum3-Labs/stacks-builder/backend/internal/api/middleware"
	"github.com/Quantum3-Labs/stacks-builder/backend/internal/database"
	"github.com/Quantum3-Labs/stacks-builder/backend/internal/querylog"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

// @title           Stacks Builder Authentication API
// @version         1.0
// @description     Authentication and API Key Management for Stacks Builder

// @license.name  MIT
// @license.url   https://opensource.org/licenses/MIT

// @host      localhost:8080
// @BasePath  /api/v1

// @securityDefinitions.basic  BasicAuth

// @externalDocs.description  OpenAPI
// @externalDocs.url          https://swagger.io/resources/open-api/

// isDataDirEmpty checks if the /data directory is empty or doesn't exist
func isDataDirEmpty(dataDir string) bool {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		// Directory doesn't exist or can't be read
		return true
	}
	return len(entries) == 0
}

func resolveDataDirectories() (string, string) {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	chromaDBDir := os.Getenv("CHROMADB_PATH")
	if chromaDBDir == "" {
		chromaDBDir = "data/chromadb"
	}

	return dataDir, chromaDBDir
}

// runPythonScript executes a Python script
func runPythonScript(scriptPath string, args ...string) error {
	pythonExec := os.Getenv("PYTHON_EXECUTABLE")
	if pythonExec == "" {
		pythonExec = "python3"
	}

	log.Printf("Running: %s %s %v", pythonExec, scriptPath, args)

	cmdArgs := append([]string{scriptPath}, args...)
	cmd := exec.Command(pythonExec, cmdArgs...)

	// Capture output
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// initializeDataIfNeeded checks if data directory is empty and runs initialization scripts
func initializeDataIfNeeded() error {
	// Get directories from environment variables
	dataDir, chromaDBDir := resolveDataDirectories()

	cloneReposScript := os.Getenv("PYTHON_CLONE_SCRIPT")
	if cloneReposScript == "" {
		cloneReposScript = "scripts/clone_repos.py" // fallback
	}

	cloneDocsScript := os.Getenv("PYTHON_CLONE_DOCS_SCRIPT")
	if cloneDocsScript == "" {
		cloneDocsScript = "scripts/clone_docs.py" // fallback
	}

	ingestSamplesScript := os.Getenv("PYTHON_INGEST_SAMPLES_SCRIPT")
	if ingestSamplesScript == "" {
		ingestSamplesScript = "scripts/ingest_samples.py" // fallback
	}

	ingestDocsScript := os.Getenv("PYTHON_INGEST_DOCS_SCRIPT")
	if ingestDocsScript == "" {
		ingestDocsScript = "scripts/ingest_docs.py" // fallback
	}

	log.Printf("Using data directory: %s", dataDir)
	log.Printf("Using ChromaDB directory: %s", chromaDBDir)

	// Check if data directory is empty
	if isDataDirEmpty(dataDir) || isDataDirEmpty(chromaDBDir) {
		log.Println("Data directory is empty. Initializing...")

		// Run clone_repos.py
		log.Println("Cloning Clarity code samples...")
		if err := runPythonScript(cloneReposScript); err != nil {
			return err
		}
		log.Println("Code samples cloned successfully")

		// Run clone_docs.py
		log.Println("Cloning Clarity documentation...")
		if err := runPythonScript(cloneDocsScript); err != nil {
			return err
		}
		log.Println("Documentation cloned successfully")

		// Run ingest_samples.py
		log.Println("Ingesting code samples into ChromaDB...")
		if err := runPythonScript(ingestSamplesScript); err != nil {
			return err
		}
		log.Println("Code samples ingestion completed successfully")

		// Run ingest_docs.py
		log.Println("Ingesting documentation into ChromaDB...")
		if err := runPythonScript(ingestDocsScript); err != nil {
			return err
		}
		log.Println("Documentation ingestion completed successfully")
	} else {
		log.Println("Data directory already initialized, skipping initialization")
	}

	return nil
}

// configureSwagger updates the generated swagger spec with the public backend URL.
func configureSwagger() {
	const defaultURL = "http://localhost:8080"

	publicURL := os.Getenv("PUBLIC_BACKEND_URL")
	if publicURL == "" {
		publicURL = defaultURL
	}

	parsedURL, err := url.Parse(publicURL)
	if err != nil || parsedURL.Host == "" {
		log.Printf("Warning: invalid PUBLIC_BACKEND_URL=%q, falling back to %s", publicURL, defaultURL)
		parsedURL, _ = url.Parse(defaultURL)
	}

	docs.SwaggerInfo.Host = parsedURL.Host

	if parsedURL.Scheme != "" {
		docs.SwaggerInfo.Schemes = []string{parsedURL.Scheme}
	} else {
		docs.SwaggerInfo.Schemes = []string{"http"}
	}
}

func main() {
	// Load environment variables from .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("Info: .env file not found, using environment variables from system")
	}

	const initMessage = "Backend is initializing data. Please try again shortly."
	dataDir, chromaDBDir := resolveDataDirectories()
	needsInitialization := isDataDirEmpty(dataDir) || isDataDirEmpty(chromaDBDir)

	if needsInitialization {
		middleware.SetMaintenanceMode(true, initMessage)
	} else {
		middleware.SetMaintenanceMode(false)
	}

	go func() {
		if err := initializeDataIfNeeded(); err != nil {
			log.Printf("Failed to initialize data: %v", err)
			middleware.SetMaintenanceMode(true, "Initialization failed. Please check server logs.")
			return
		}
		middleware.SetMaintenanceMode(false)
	}()

	// Configure swagger host/scheme for the current environment
	configureSwagger()

	// Initialize database
	db, err := database.InitDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Initialize query logging service
	qr := querylog.NewRepository(db)
	qs := querylog.NewService(qr)

	// Set Gin mode
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.DebugMode)
	}

	// Create Gin router
	router := gin.Default()
	router.Use(middleware.MaintenanceModeMiddleware())

	// Setup routes
	api.SetupRoutes(router, db, qr, qs)

	// Get port from environment or use default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Create HTTP server
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Channel to receive shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start server in goroutine
	go func() {
		log.Printf("Server starting on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sig := <-quit
	log.Printf("Received signal: %v. Initiating graceful shutdown...", sig)

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown HTTP server (stops accepting new requests, waits for in-flight)
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	} else {
		log.Println("HTTP server stopped")
	}

	// Flush async query logs
	qs.FlushAndWait(5 * time.Second)

	// Close database (includes WAL checkpoint)
	if err := database.Close(); err != nil {
		log.Printf("Database close error: %v", err)
	}

	log.Println("Graceful shutdown complete")
}
