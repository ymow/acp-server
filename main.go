package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/inkmesh/acp-server/internal/api"
	acpcrypto "github.com/inkmesh/acp-server/internal/crypto"
	"github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/keys"
	"github.com/inkmesh/acp-server/internal/reencrypt"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		runServe()
	case "rotate-key":
		runRotateKey()
	case "reencrypt":
		runReencrypt(os.Args[2:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "acp-server: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `acp-server — Agent Covenant Protocol reference server

Usage:
  acp-server [serve]           Run the HTTP server (default).
  acp-server rotate-key        Generate the next keyring version and bump
                               the active key pointer. Existing ciphertext
                               stays readable via its recorded key_version.
  acp-server reencrypt         Re-encrypt rows sealed under older versions
                               into the current version. Idempotent.

Env:
  ACP_DB_PATH   SQLite path. Default: acp.db
  ACP_ADDR      Listen address. Default: :8080
  ACP_KEY_FILE  Absolute path to the legacy keyfile anchor. The keyring
                directory lives at its sibling "keys/" subdir.
                Default: $HOME/.acp/master.key
`)
}

func runServe() {
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

func runRotateKey() {
	p, err := keys.NewLocalKeyfileProvider("")
	if err != nil {
		log.Fatalf("open keyring: %v", err)
	}
	version, fp, err := p.Rotate()
	if err != nil {
		log.Fatalf("rotate: %v", err)
	}
	fmt.Printf("rotated: new key_version=%d fingerprint=%s keyring=%s\n",
		version, fp, p.KeyringDir())
	fmt.Println("Existing ciphertext remains readable. Run `acp-server reencrypt`")
	fmt.Println("to re-seal rows under the new version.")
}

func runReencrypt(_ []string) {
	dbPath := os.Getenv("ACP_DB_PATH")
	if dbPath == "" {
		dbPath = "acp.db"
	}
	conn, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", dbPath, err)
	}
	defer conn.Close()

	p, err := keys.NewLocalKeyfileProvider("")
	if err != nil {
		log.Fatalf("open keyring: %v", err)
	}
	sealer := acpcrypto.NewSealer(p)

	stats, err := reencrypt.Run(conn, sealer)
	if err != nil {
		log.Fatalf("reencrypt: %v (partial: scanned=%d reencrypted=%d skipped=%d null=%d)",
			err, stats.Scanned, stats.Reencrypted, stats.Skipped, stats.NullEnc)
	}
	fmt.Printf("reencrypt complete: scanned=%d reencrypted=%d skipped=%d null_enc=%d\n",
		stats.Scanned, stats.Reencrypted, stats.Skipped, stats.NullEnc)
	for table, ts := range stats.PerTable {
		fmt.Printf("  %-24s scanned=%d reencrypted=%d skipped=%d null_enc=%d\n",
			table, ts.Scanned, ts.Reencrypted, ts.Skipped, ts.NullEnc)
	}
}
