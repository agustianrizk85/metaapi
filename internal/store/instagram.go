package store

import "time"

// IGAccount is one Instagram professional account connected via the "Instagram
// API with Instagram Login". Unlike the Facebook-login path (System User + Page
// token), each IG account carries its own user access token. The token is
// long-lived (60 days) and refreshed in place by the server before it expires,
// so in practice it never dies — the team pastes it once.
type IGAccount struct {
	ID             uint       `gorm:"primaryKey" json:"connId"`
	IGUserID       string     `gorm:"uniqueIndex;size:64" json:"id"` // IG account id; recipient/from resolve against this
	Username       string     `gorm:"size:160" json:"username"`
	Name           string     `gorm:"size:160" json:"name"`
	ProfilePicture string     `gorm:"type:text" json:"profile_picture_url"` // IG CDN URLs can exceed 512 chars
	Followers      int        `json:"followers_count"`
	MediaCount     int        `json:"media_count"`
	AccessToken    string     `gorm:"type:text" json:"-"` // server-side only, never serialised
	TokenExpiresAt *time.Time `json:"token_expires_at"`
	RefreshedAt    *time.Time `json:"refreshed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ListIGAccounts returns every connected Instagram account.
func (s *Store) ListIGAccounts() ([]IGAccount, error) {
	var out []IGAccount
	return out, s.db.Order("username asc").Find(&out).Error
}

// FindIGAccount looks up a connected account by its IG user id.
func (s *Store) FindIGAccount(igUserID string) (*IGAccount, error) {
	var a IGAccount
	if err := s.db.Where("ig_user_id = ?", igUserID).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// UpsertIGAccount creates or updates an account keyed by IG user id (re-pasting a
// token for the same account refreshes its row instead of duplicating).
func (s *Store) UpsertIGAccount(a *IGAccount) error {
	var existing IGAccount
	if err := s.db.Where("ig_user_id = ?", a.IGUserID).First(&existing).Error; err == nil {
		a.ID = existing.ID
		a.CreatedAt = existing.CreatedAt
		return s.db.Save(a).Error
	}
	return s.db.Create(a).Error
}

// SaveIGAccount persists an existing account (used by the token refresher).
func (s *Store) SaveIGAccount(a *IGAccount) error { return s.db.Save(a).Error }

// DeleteIGAccount removes a connected account by primary key.
func (s *Store) DeleteIGAccount(id uint) error { return s.db.Delete(&IGAccount{}, id).Error }
