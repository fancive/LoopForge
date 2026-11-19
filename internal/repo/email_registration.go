package repo

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type emailRegistrationRow struct {
	Email     string    `gorm:"primaryKey;size:254"`
	Digest    string    `gorm:"size:64;not null"`
	ExpiresAt time.Time `gorm:"not null;index"`
	Attempts  int       `gorm:"not null"`
}

func (emailRegistrationRow) TableName() string { return "email_registrations" }

const emailCodeAttempts = 5

var ErrEmailCodeInvalid = errors.New("invalid or expired email code")

// IssueEmailRegistration reserves one send per address within the cooldown.
// The unique address and conditional update also apply across API servers.
func (r *UsersRepo) IssueEmailRegistration(email, digest string, expires time.Time, cooldown time.Duration) (bool, error) {
	row := emailRegistrationRow{Email: email, Digest: digest, ExpiresAt: expires}
	err := r.DB.Create(&row).Error
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return err == nil, err
	}
	result := r.DB.Model(&emailRegistrationRow{}).
		Where("email = ? AND expires_at <= ?", email, expires.Add(-cooldown)).
		Updates(map[string]any{"digest": digest, "expires_at": expires, "attempts": 0})
	return result.RowsAffected > 0, result.Error
}

func (r *UsersRepo) CancelEmailRegistration(email, digest string) error {
	return r.DB.Where("email = ? AND digest = ?", email, digest).Delete(&emailRegistrationRow{}).Error
}

func (r *UsersRepo) UserByVerifiedEmail(email string) (domain.User, bool) {
	var account localAccountRow
	if err := r.DB.Where("verified_email = ?", email).First(&account).Error; err != nil {
		return domain.User{}, false
	}
	return r.User(account.UserID)
}

// CreateVerifiedLocalAccount consumes the code and inserts the account in one
// transaction. Invalid guesses are committed so a new process cannot reset them.
func (r *UsersRepo) CreateVerifiedLocalAccount(email, password, digest string, now time.Time) (domain.User, error) {
	// The email is the sign-in identifier. A separate stable handle keeps
	// account references within the existing username format.
	key := sha256.Sum256([]byte(email))
	login := fmt.Sprintf("mail-%x", key[:16])
	displayName, _, _ := strings.Cut(email, "@")
	displayName = displayName[:min(64, len(displayName))]
	if len(password) < MinPasswordLength || len(password) > MaxPasswordLength {
		return domain.User{}, errPasswordLength
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return domain.User{}, err
	}
	var user userRow
	var verified bool
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&emailRegistrationRow{}).
			Where("email = ? AND expires_at > ? AND attempts < ?", email, now, emailCodeAttempts).
			UpdateColumn("attempts", gorm.Expr("attempts + 1"))
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		var matches int64
		if err := tx.Model(&emailRegistrationRow{}).Where("email = ? AND digest = ?", email, digest).Count(&matches).Error; err != nil || matches == 0 {
			return err
		}
		if err := tx.Model(&emailRegistrationRow{}).Where("email = ?", email).UpdateColumn("attempts", emailCodeAttempts).Error; err != nil {
			return err
		}
		verified = true
		var existing int64
		if err := tx.Model(&userRow{}).Where("LOWER(login_name) = LOWER(?)", login).Count(&existing).Error; err != nil {
			return err
		}
		if existing != 0 {
			return ErrLoginTaken
		}
		user = userRow{LoginName: login, ChineseName: displayName, Created: &now, Updated: &now}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return tx.Create(&localAccountRow{UserID: user.ID, PasswordHash: string(hash), VerifiedEmail: &email}).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return domain.User{}, ErrLoginTaken
	}
	if err != nil {
		return domain.User{}, err
	}
	if !verified {
		return domain.User{}, ErrEmailCodeInvalid
	}
	return domain.User{ID: user.ID, LoginName: login, ChineseName: displayName}, nil
}
