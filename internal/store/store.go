// Package store persists WhatsApp conversations. The Graph API exposes no
// message-history endpoint for WhatsApp (unlike Instagram), so the only way to
// show an inbox is to capture every inbound message from the Cloud API webhook
// and every outbound reply we send, then read them back from here.
package store

import (
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// WAMessage is a single WhatsApp message, inbound or outbound. WamID is the
// WhatsApp-assigned message id used to dedupe webhook redeliveries.
type WAMessage struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	WamID         string    `gorm:"uniqueIndex;size:128" json:"wamId"`
	PhoneNumberID string    `gorm:"index;size:64" json:"phoneNumberId"` // our business number
	WabaID        string    `gorm:"size:64" json:"wabaId"`
	ContactWaID   string    `gorm:"index;size:32" json:"contactWaId"` // customer phone (no +)
	ContactName   string    `gorm:"size:160" json:"contactName"`
	Direction     string    `gorm:"size:8" json:"direction"` // "in" | "out"
	Type          string    `gorm:"size:24" json:"type"`     // text, image, …
	Text          string    `gorm:"type:text" json:"text"`
	Status        string    `gorm:"size:24" json:"status"` // sent/delivered/read/failed (outbound)
	FailReason    string    `gorm:"column:fail_reason;type:text" json:"error"` // alasan gagal dari Meta (kode + judul)
	Timestamp     time.Time `gorm:"index" json:"timestamp"`
	CreatedAt     time.Time `json:"createdAt"`
}

// WAConversation is a denormalised per-customer thread, updated on every
// message so the inbox list is a single ordered SELECT (no GROUP BY gymnastics).
type WAConversation struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	PhoneNumberID string    `gorm:"index:idx_wa_conv,unique;size:64" json:"phoneNumberId"`
	ContactWaID   string    `gorm:"index:idx_wa_conv,unique;size:32" json:"contactWaId"`
	ContactName   string    `gorm:"size:160" json:"contactName"`
	LastSnippet   string    `gorm:"type:text" json:"lastSnippet"`
	LastDirection string    `gorm:"size:8" json:"lastDirection"`
	LastMessageAt time.Time `gorm:"index" json:"lastMessageAt"`
	Unread        int       `json:"unread"`
}

type Store struct{ db *gorm.DB }

// Open opens the store and migrates the schema. `dsn` selects the driver: a
// Postgres DSN (postgres://… or a libpq key=value string) uses Postgres — the
// central, persistent store shared with the rest of Greenpark; anything else is
// treated as a SQLite file path (embedded fallback / local dev).
func Open(dsn string) (*Store, error) {
	var dial gorm.Dialector
	if isPostgresDSN(dsn) {
		dial = postgres.Open(dsn)
	} else {
		dial = sqlite.Open(dsn)
	}
	db, err := gorm.Open(dial, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&WAMessage{}, &WAConversation{}, &MetaConnection{}, &MetaAppConfig{}, &IGAccount{}, &WAAISetting{}); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// isPostgresDSN reports whether dsn names a Postgres database rather than a
// SQLite file. It recognises both URL form (postgres://…, postgresql://…) and
// the libpq key=value form (host=… user=…).
func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") ||
		strings.HasPrefix(dsn, "postgresql://") ||
		(strings.Contains(dsn, "host=") && strings.Contains(dsn, "user="))
}

// SaveIncoming stores an inbound message (idempotent on WamID) and bumps the
// conversation's snippet + unread counter. Returns true when it was new.
func (s *Store) SaveIncoming(m *WAMessage) (bool, error) {
	m.Direction = "in"
	res := s.db.Clauses(clause.OnConflict{DoNothing: true, Columns: []clause.Column{{Name: "wam_id"}}}).Create(m)
	if res.Error != nil {
		return false, res.Error
	}
	isNew := res.RowsAffected > 0
	if isNew {
		s.touchConversation(m, true)
	}
	return isNew, nil
}

// SaveOutgoing stores a reply we sent and clears the unread counter.
func (s *Store) SaveOutgoing(m *WAMessage) error {
	m.Direction = "out"
	if err := s.db.Create(m).Error; err != nil {
		return err
	}
	s.touchConversation(m, false)
	return nil
}

// touchConversation upserts the denormalised thread row for a message.
func (s *Store) touchConversation(m *WAMessage, incoming bool) {
	var conv WAConversation
	s.db.Where("phone_number_id = ? AND contact_wa_id = ?", m.PhoneNumberID, m.ContactWaID).First(&conv)
	conv.PhoneNumberID = m.PhoneNumberID
	conv.ContactWaID = m.ContactWaID
	if m.ContactName != "" {
		conv.ContactName = m.ContactName
	}
	conv.LastSnippet = m.Text
	conv.LastDirection = m.Direction
	conv.LastMessageAt = m.Timestamp
	if incoming {
		conv.Unread++
	} else {
		conv.Unread = 0
	}
	s.db.Save(&conv)
}

// UpdateStatus applies a delivery-status callback to an outbound message. When
// the status is "failed", reason carries Meta's error (code + title) so the UI
// can show WHY it failed; an empty reason never overwrites an existing one.
func (s *Store) UpdateStatus(wamID, status, reason string) error {
	updates := map[string]any{"status": status}
	if reason != "" {
		updates["fail_reason"] = reason
	}
	return s.db.Model(&WAMessage{}).Where("wam_id = ?", wamID).Updates(updates).Error
}

// Conversations lists threads newest-first, optionally scoped to one of our
// phone numbers.
func (s *Store) Conversations(phoneNumberID string) ([]WAConversation, error) {
	var out []WAConversation
	q := s.db.Order("last_message_at DESC")
	if phoneNumberID != "" {
		q = q.Where("phone_number_id = ?", phoneNumberID)
	}
	return out, q.Find(&out).Error
}

// Messages returns one thread's history in chronological order (oldest first),
// capped at the most recent `limit`.
func (s *Store) Messages(phoneNumberID, contactWaID string, limit int) ([]WAMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	var rows []WAMessage
	err := s.db.Where("phone_number_id = ? AND contact_wa_id = ?", phoneNumberID, contactWaID).
		Order("timestamp DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// MarkRead zeroes a thread's unread counter.
func (s *Store) MarkRead(phoneNumberID, contactWaID string) error {
	return s.db.Model(&WAConversation{}).
		Where("phone_number_id = ? AND contact_wa_id = ?", phoneNumberID, contactWaID).
		Update("unread", 0).Error
}
