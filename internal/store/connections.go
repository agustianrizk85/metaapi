package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrNotFound is returned when a connection id doesn't exist.
var ErrNotFound = errors.New("not found")

// MetaConnection is one connected Meta account (one System User token, or one
// OAuth login). A token may grant access to several ad accounts / pages / WABAs.
// metaapi aggregates DATA across every connection, so connecting more accounts
// here makes Ads / WhatsApp / Instagram show the combined portfolio. Exactly one
// is flagged active (used for deep single-account breakdowns).
type MetaConnection struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Label          string     `gorm:"size:160" json:"label"`
	MetaUserID     string     `gorm:"size:64;index" json:"meta_user_id"`
	MetaUserName   string     `gorm:"size:160" json:"meta_user_name"`
	AccessToken    string     `gorm:"size:1024" json:"-"` // server-side only
	TokenExpiresAt *time.Time `json:"token_expires_at"`
	BusinessID     string     `gorm:"size:64" json:"business_id"`
	AdAccountID    string     `gorm:"size:64" json:"ad_account_id"`
	Scopes         string     `gorm:"size:512" json:"scopes"`
	IsActive       bool       `gorm:"index" json:"is_active"`
	CreatedBy      uint       `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// MetaAppConfig holds the OAuth app credentials (singleton row id=1). Kept so the
// "Akun Meta" UI can show readiness; the paste-token flow doesn't require it.
type MetaAppConfig struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	AppID       string    `gorm:"size:64" json:"app_id"`
	AppSecret   string    `gorm:"size:255" json:"-"`
	RedirectURI string    `gorm:"size:255" json:"redirect_uri"`
	APIVersion  string    `gorm:"size:16" json:"api_version"`
	Scopes      string    `gorm:"size:512" json:"scopes"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const metaAppConfigID = 1

// ListConnections returns every connected account, active first then newest.
func (s *Store) ListConnections() ([]MetaConnection, error) {
	var out []MetaConnection
	err := s.db.Order("is_active DESC, created_at DESC").Find(&out).Error
	return out, err
}

func (s *Store) CountConnections() (int64, error) {
	var n int64
	err := s.db.Model(&MetaConnection{}).Count(&n).Error
	return n, err
}

func (s *Store) FindConnection(id uint) (*MetaConnection, error) {
	var c MetaConnection
	err := s.db.First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) FindConnectionByMetaUserID(metaUserID string) (*MetaConnection, error) {
	var c MetaConnection
	err := s.db.Where("meta_user_id = ?", metaUserID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) CreateConnection(c *MetaConnection) error { return s.db.Create(c).Error }
func (s *Store) SaveConnection(c *MetaConnection) error   { return s.db.Save(c).Error }

// SetActive marks one connection active and clears the rest, in one transaction.
func (s *Store) SetActive(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&MetaConnection{}).Where("is_active = ?", true).Update("is_active", false).Error; err != nil {
			return err
		}
		res := tx.Model(&MetaConnection{}).Where("id = ?", id).Update("is_active", true)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteConnection removes a connection; if it was active, promotes the newest
// remaining one so aggregation/active-breakdown keep working.
func (s *Store) DeleteConnection(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var c MetaConnection
		if err := tx.First(&c, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Delete(&MetaConnection{}, id).Error; err != nil {
			return err
		}
		if c.IsActive {
			var next MetaConnection
			if err := tx.Order("created_at DESC").First(&next).Error; err == nil {
				return tx.Model(&MetaConnection{}).Where("id = ?", next.ID).Update("is_active", true).Error
			}
		}
		return nil
	})
}

// AppConfig returns the singleton config row, creating it on first access.
func (s *Store) AppConfig() (*MetaAppConfig, error) {
	var cfg MetaAppConfig
	err := s.db.First(&cfg, metaAppConfigID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		cfg = MetaAppConfig{ID: metaAppConfigID}
		if err := s.db.Create(&cfg).Error; err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	return &cfg, err
}

func (s *Store) SaveAppConfig(cfg *MetaAppConfig) error {
	cfg.ID = metaAppConfigID
	return s.db.Save(cfg).Error
}
