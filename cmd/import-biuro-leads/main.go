package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dzebovski/kolss-platform-api/internal/biuroimport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-biuro-leads:", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", string(biuroimport.ModeDryRun), "dry-run or apply")
	inputPath := flag.String("input", "-", "CSV input path, or - for stdin")
	expectedSHA := flag.String("expected-sha256", "", "required in apply mode")
	spreadsheetID := flag.String("spreadsheet-id", biuroimport.DefaultSpreadsheetID, "source Google spreadsheet id")
	sheetID := flag.Int64("sheet-id", biuroimport.DefaultSheetID, "source Google sheet id")
	sheetName := flag.String("sheet-name", biuroimport.DefaultSheetName, "source Google sheet name")
	officeCode := flag.String("office-code", biuroimport.DefaultOfficeCode, "target office code")
	timeout := flag.Duration("timeout", 2*time.Minute, "database operation timeout")
	flag.Parse()

	var input io.Reader = os.Stdin
	var file *os.File
	var err error
	if *inputPath != "-" {
		file, err = os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open input: %w", err)
		}
		defer file.Close()
		input = file
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	source, err := biuroimport.Parse(data, biuroimport.SourceConfig{
		SpreadsheetID: strings.TrimSpace(*spreadsheetID),
		SheetID:       *sheetID,
		SheetName:     strings.TrimSpace(*sheetName),
		OfficeCode:    strings.TrimSpace(*officeCode),
	})
	if err != nil {
		return err
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	baseContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(baseContext, *timeout)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// Supabase's transaction pooler does not support named prepared statements.
	poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	report, runErr := biuroimport.Run(ctx, pool, source, biuroimport.RunOptions{
		Mode:           biuroimport.Mode(*mode),
		ExpectedSHA256: *expectedSHA,
	})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if runErr != nil {
		return runErr
	}
	return nil
}
