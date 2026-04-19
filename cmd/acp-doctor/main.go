// Command acp-doctor runs offline integrity checks against an acp-server
// database and exits non-zero if any Error-severity finding is produced.
//
// Usage:
//
//	acp-doctor                 # uses $ACP_DB_PATH or ./acp.db
//	acp-doctor -db /path/acp.db
//
// Phase 4.5.7 ships platform_id residual scans (ACR-700 §4). Future phases
// add more checks behind the same CLI so operators and CI have one tool.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/doctor"
)

func main() {
	var dbPath string
	flag.StringVar(&dbPath, "db", "", "path to acp-server sqlite database (default: $ACP_DB_PATH or ./acp.db)")
	flag.Parse()

	if dbPath == "" {
		dbPath = os.Getenv("ACP_DB_PATH")
	}
	if dbPath == "" {
		dbPath = "acp.db"
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", dbPath, err)
	}
	defer conn.Close()

	fmt.Printf("acp-doctor scanning %s\n", dbPath)
	report := doctor.Run(conn)
	report.Print(os.Stdout)
	os.Exit(report.ExitCode())
}
