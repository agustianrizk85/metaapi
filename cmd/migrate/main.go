// Command migrate copies the metaapi SQLite database into Postgres — a one-shot
// data move for the SQLite→Postgres migration. It reads every row from the old
// SQLite file (WA_DB_PATH) and inserts it into the Postgres DB (WA_DATABASE_URL),
// then prints a summary. The summary doubles as an auto-reply diagnostic: it
// shows the wa_ai_settings toggle and the inbound/outbound message counts, so
// you can see at a glance whether auto-reply was ever eligible to fire.
//
//	WA_DB_PATH=./metaapi.db WA_DATABASE_URL=postgres://… go run ./cmd/migrate
//
// It is idempotent for the singleton settings row (kept at id=1); message and
// connection rows are re-inserted with fresh ids, so run it once into an empty
// Postgres DB. Existing rows in Postgres are left untouched.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"metaapi/internal/store"
)

func main() {
	srcPath := envOr("WA_DB_PATH", "./metaapi.db")
	dstDSN := os.Getenv("WA_DATABASE_URL")
	if dstDSN == "" {
		log.Fatal("WA_DATABASE_URL kosong — set DSN Postgres tujuan (postgres://user:pass@host:5432/greenpark_meta)")
	}

	src, err := gorm.Open(sqlite.Open(srcPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		log.Fatalf("buka SQLite sumber (%s): %v", srcPath, err)
	}
	dst, err := gorm.Open(postgres.Open(dstDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		log.Fatalf("buka Postgres tujuan: %v", err)
	}
	if err := dst.AutoMigrate(&store.WAMessage{}, &store.WAConversation{}, &store.MetaConnection{}, &store.MetaAppConfig{}, &store.IGAccount{}, &store.WAAISetting{}); err != nil {
		log.Fatalf("automigrate Postgres: %v", err)
	}

	fmt.Println("=== migrasi metaapi SQLite → Postgres ===")
	fmt.Printf("sumber : %s\n", srcPath)

	// Messages & conversations & connections: no cross-table foreign keys, so we
	// drop the old ids and let Postgres assign fresh ones (keeps the sequence in
	// sync so new webhook inserts don't collide). Read in id order to preserve
	// chronology.
	msgIn, msgOut := copyMessages(src, dst)
	convN := copyGeneric[store.WAConversation](src, dst)
	connN := copyGeneric[store.MetaConnection](src, dst)
	appN := copyGeneric[store.MetaAppConfig](src, dst)
	igN := copyGeneric[store.IGAccount](src, dst)

	// Settings singleton: keep id=1 (the code reads it by that fixed id) and
	// upsert so re-runs stay idempotent.
	var settings []store.WAAISetting
	src.Find(&settings)
	for i := range settings {
		if err := dst.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).Create(&settings[i]).Error; err != nil {
			log.Fatalf("salin wa_ai_settings: %v", err)
		}
	}

	fmt.Println("--- hasil ---")
	fmt.Printf("wa_messages   : %d (in=%d, out=%d)\n", msgIn+msgOut, msgIn, msgOut)
	fmt.Printf("conversations : %d\n", convN)
	fmt.Printf("connections   : %d\n", connN)
	fmt.Printf("app_config    : %d\n", appN)
	fmt.Printf("ig_accounts   : %d\n", igN)

	fmt.Println("--- diagnosa auto-reply ---")
	if len(settings) == 0 {
		fmt.Println("wa_ai_settings: TIDAK ADA baris → toggle auto_reply belum pernah diset (default OFF).")
	}
	for _, st := range settings {
		fmt.Printf("wa_ai_settings: auto_reply=%v model=%q prompt_len=%d\n", st.AutoReply, st.Model, len(st.Prompt))
		if !st.AutoReply {
			fmt.Println("  → auto_reply OFF: inilah kenapa WA tak pernah balas otomatis. Nyalakan toggle di UI (atau UPDATE di Postgres).")
		}
	}
	if msgIn == 0 {
		fmt.Println("wa_messages: TIDAK ADA pesan masuk (in=0) → webhook 'messages' Meta belum mengantar pesan ke metaapi (cek langganan webhook / nomor Cloud API).")
	}
	fmt.Println("=== selesai ===")
}

// copyMessages copies WAMessage rows with fresh ids (in chronological order) and
// returns the inbound/outbound counts.
func copyMessages(src, dst *gorm.DB) (in, out int) {
	var rows []store.WAMessage
	src.Order("id asc").Find(&rows)
	for i := range rows {
		rows[i].ID = 0
		if rows[i].Direction == "in" {
			in++
		} else {
			out++
		}
	}
	if len(rows) > 0 {
		if err := dst.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 200).Error; err != nil {
			log.Fatalf("salin wa_messages: %v", err)
		}
	}
	return in, out
}

// copyGeneric copies every row of a table with fresh ids (no cross-table FKs).
func copyGeneric[T any](src, dst *gorm.DB) int {
	var rows []T
	src.Order("id asc").Find(&rows)
	for i := range rows {
		zeroID(&rows[i])
	}
	if len(rows) > 0 {
		if err := dst.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 200).Error; err != nil {
			log.Fatalf("salin %T: %v", rows, err)
		}
	}
	return len(rows)
}

// zeroID sets the row's primary key to 0 so Postgres assigns a fresh id. Each
// migrated model exposes ID as a uint field.
func zeroID(v any) {
	switch r := v.(type) {
	case *store.WAConversation:
		r.ID = 0
	case *store.MetaConnection:
		r.ID = 0
	case *store.MetaAppConfig:
		r.ID = 0
	case *store.IGAccount:
		r.ID = 0
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
