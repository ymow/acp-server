package main

import (
	"log"
	"net/http"
	"os"

	"github.com/inkmesh/acp-server/internal/api"
	"github.com/inkmesh/acp-server/internal/db"
)

func main() {
	dbPath := os.Getenv("ACP_DB_PATH")
	if dbPath == "" {
		dbPath = "acp.db"
	}
	addr := os.Getenv("ACP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", dbPath, err)
	}
	defer conn.Close()

	srv := api.New(conn)
	log.Printf("acp-server listening on %s  db=%s", addr, dbPath)
	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatal(err)
	}
}
