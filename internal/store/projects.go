package store

import (
	"time"

	"gorm.io/gorm"
)

// Project (Proyek) links a real-estate project to the Meta accounts (WhatsApp
// numbers / Instagram accounts) that serve it and the sales team that handles
// it. It is the mapping layer behind three features: attributing inbound
// chats/leads to a project + salesperson, routing to that project's sales, and
// filtering the dashboard per project.
type Project struct {
	ID        uint             `gorm:"primaryKey" json:"id"`
	Name      string           `gorm:"size:160;index" json:"name"`
	Note      string           `gorm:"type:text" json:"note"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
	Accounts  []ProjectAccount `gorm:"foreignKey:ProjectID" json:"accounts"`
	Sales     []ProjectSales   `gorm:"foreignKey:ProjectID" json:"sales"`
}

// ProjectAccount ties one Meta account to a project. Ref is the stable id we can
// match inbound events against: a WhatsApp phone_number_id ("wa") or an
// Instagram user id ("ig"). Label is the human display (phone / @username).
type ProjectAccount struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ProjectID uint   `gorm:"index" json:"projectId"`
	Kind      string `gorm:"size:8;index" json:"kind"` // "wa" | "ig"
	Ref       string `gorm:"size:64;index" json:"ref"` // wa phone_number_id | ig user id
	Label     string `gorm:"size:160" json:"label"`
}

// ProjectSales is one salesperson assigned to a project, keyed by their login
// e-mail (matches the unified-auth identity) so attribution survives renames.
type ProjectSales struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ProjectID uint   `gorm:"index" json:"projectId"`
	Email     string `gorm:"size:160;index" json:"email"`
	Name      string `gorm:"size:160" json:"name"`
}

// ListProjects returns every project with its accounts + sales, ordered by name.
func (s *Store) ListProjects() ([]Project, error) {
	var ps []Project
	err := s.db.Preload("Accounts").Preload("Sales").Order("name asc").Find(&ps).Error
	return ps, err
}

// SaveProject upserts a project (by ID) and REPLACES its account + sales sets
// with the ones supplied — the admin UI always sends the full desired state.
func (s *Store) SaveProject(p *Project) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		accounts, sales := p.Accounts, p.Sales
		p.Accounts, p.Sales = nil, nil // manage the links explicitly, not via assoc
		if p.ID == 0 {
			if err := tx.Create(p).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Model(&Project{}).Where("id = ?", p.ID).
				Updates(map[string]any{"name": p.Name, "note": p.Note}).Error; err != nil {
				return err
			}
			if err := tx.Where("project_id = ?", p.ID).Delete(&ProjectAccount{}).Error; err != nil {
				return err
			}
			if err := tx.Where("project_id = ?", p.ID).Delete(&ProjectSales{}).Error; err != nil {
				return err
			}
		}
		for i := range accounts {
			accounts[i].ID, accounts[i].ProjectID = 0, p.ID
		}
		for i := range sales {
			sales[i].ID, sales[i].ProjectID = 0, p.ID
		}
		if len(accounts) > 0 {
			if err := tx.Create(&accounts).Error; err != nil {
				return err
			}
		}
		if len(sales) > 0 {
			if err := tx.Create(&sales).Error; err != nil {
				return err
			}
		}
		p.Accounts, p.Sales = accounts, sales
		return nil
	})
}

// DeleteProject removes a project and its links.
func (s *Store) DeleteProject(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("project_id = ?", id).Delete(&ProjectAccount{}).Error; err != nil {
			return err
		}
		if err := tx.Where("project_id = ?", id).Delete(&ProjectSales{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Project{}, id).Error
	})
}

// ProjectForAccount resolves the project (with its sales team) that owns a given
// account — used to attribute an inbound WhatsApp/Instagram message. kind is
// "wa" (ref = phone_number_id) or "ig" (ref = ig user id). Returns nil,nil when
// the account is not mapped to any project (no error — unmapped is normal).
func (s *Store) ProjectForAccount(kind, ref string) (*Project, error) {
	var pa ProjectAccount
	err := s.db.Where("kind = ? AND ref = ?", kind, ref).First(&pa).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Project
	if err := s.db.Preload("Sales").Preload("Accounts").First(&p, pa.ProjectID).Error; err != nil {
		return nil, err
	}
	return &p, nil
}
