// Package links stores customer links by token digest and owns their life from staff issuance through drafts, final submit, agreement acceptance and cancellation. Enroll, called inside billing's funding transaction, turns a paid link into a Student. See docs/domain/links.md.
package links

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"projecthermes/internal/settings"
	"projecthermes/internal/students"
)

// pinIndex names Link.Pin's unique index; the struct tag below cannot reference it.
const pinIndex = "idx_link_pin"

const tokenBytes = 32

// Link stores only the token's digest, so a database leak yields no usable URL.
type Link struct {
	TokenHash []byte `gorm:"primaryKey;not null;check:token_hash_size,octet_length(token_hash) = 32"`
	students.Details
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// OpenedAt is the customer's first visit.
	OpenedAt *time.Time
	// SubmittedAt is the customer's final submit; autosave never sets it.
	SubmittedAt *time.Time
	// TermsAcceptedAt and ProgramStartDate retain the agreement first accepted.
	TermsAcceptedAt  *time.Time `gorm:"type:timestamptz"`
	ProgramStartDate string     `gorm:"type:text;not null;default:''"`
	// Pin is the PIN chosen at submit. It reserves the PIN until payment moves it to the Student; "" before submit and after payment.
	// The index name must equal pinIndex.
	Pin string `gorm:"not null;default:'';uniqueIndex:idx_link_pin,where:pin <> ''"`
	// PaidAt is when the first payment was confirmed: the instant Enroll created the Student. Nil while unpaid.
	PaidAt *time.Time `gorm:"type:timestamptz"`
	// CanceledAt revokes an unfinished token but keeps its draft for staff.
	CanceledAt *time.Time
	// ExpiresAt starts with the configured lifetime; Submit replaces it with 168 hours from submission. Missing expiries fail closed.
	ExpiresAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// CreatedBy and CanceledBy are the staff usernames behind each action.
	// Empty audit values mean the corresponding action has not occurred.
	CreatedBy  string            `gorm:"not null;default:''"`
	CanceledBy string            `gorm:"not null;default:''"`
	Student    *students.Student `gorm:"foreignKey:SourceLinkHash;references:TokenHash;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

// Status is a link's customer progress as shown to staff.
type Status string

const (
	NotOpened Status = "Not opened"
	Opened    Status = "Opened"
	// InProgress means autosaved data exists but the customer hasn't submitted.
	InProgress Status = "In progress"
	// AwaitingPayment means the customer submitted the form; no payment confirmed yet.
	AwaitingPayment Status = "Awaiting payment"
	// Finished means payment confirmed and the Student created.
	Finished Status = "Finished"
	// Expired means the link lapsed before payment was confirmed.
	Expired  Status = "Expired"
	Canceled Status = "Canceled"
)

// SaveResult reports the authoritative outcome of a customer write.
type SaveResult uint8

const (
	Unavailable SaveResult = iota
	Saved
	AlreadySubmitted
	// PinTaken means another submitted link or enrolled student holds the PIN; the form stays open.
	PinTaken
)

// Entry is a link as shown to staff. ID is the lowercase hex token digest.
// Token is set only by Create: it is never stored, so it can't be shown again.
type Entry struct {
	ID          string
	Token       string
	Name        string
	Phone       string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	SubmittedAt *time.Time
	PaidAt      *time.Time
	CreatedBy   string
	CanceledBy  string
	Status      Status
}

// Cancelable reports whether staff can still revoke an unfinished link.
func (e Entry) Cancelable() bool {
	return e.Status == NotOpened || e.Status == Opened || e.Status == InProgress
}

// Archived reports whether payment is at least 24 hours old at now.
func (e Entry) Archived(now time.Time) bool {
	return e.Status == Finished && e.PaidAt != nil && !e.PaidAt.After(now.Add(-24*time.Hour))
}

// Trashed reports whether the link belongs in canceled or expired history.
func (e Entry) Trashed() bool {
	return e.Status == Canceled || e.Status == Expired
}

func (l Link) entry(now time.Time) Entry {
	e := Entry{
		ID: hex.EncodeToString(l.TokenHash), Name: l.Name, Phone: l.Phone, Status: NotOpened,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, CreatedBy: l.CreatedBy, CanceledBy: l.CanceledBy,
		SubmittedAt: l.SubmittedAt, PaidAt: l.PaidAt,
	}
	switch {
	case l.CanceledAt != nil:
		e.Status = Canceled
	case l.PaidAt != nil:
		e.Status = Finished // paid wins over expiry: an in-flight payment can fund after the link lapses
	case l.SubmittedAt != nil && now.Before(l.ExpiresAt):
		e.Status = AwaitingPayment
	case !now.Before(l.ExpiresAt):
		e.Status = Expired
	case l.Details != (students.Details{}):
		e.Status = InProgress
	case l.OpenedAt != nil:
		e.Status = Opened
	}
	return e
}

// errNoStaff rejects staff actions that would leave an anonymous audit entry.
var errNoStaff = errors.New("links: staff username required")

// ErrProgramStartChanged requires the customer to review the current start date.
var ErrProgramStartChanged = errors.New("links: program start date changed or unavailable")

type Store struct {
	db *gorm.DB
	// changed runs after every committed write, so open management pages refresh.
	changed func()
}

// New returns a store whose committed writes call changed, when it is non-nil.
func New(db *gorm.DB, changed func()) *Store {
	if changed == nil {
		changed = func() {}
	}
	return &Store{db: db, changed: changed}
}

// Create issues a link that stops working at expiresAt, on behalf of the
// signed-in staff username.
func (s *Store) Create(ctx context.Context, staff string, expiresAt time.Time) (Entry, error) {
	if staff == "" {
		return Entry{}, errNoStaff
	}
	var random [tokenBytes]byte
	// Go 1.26 terminates the process if the OS random source fails.
	rand.Read(random[:])
	token := base64.RawURLEncoding.EncodeToString(random[:])
	digest, _ := tokenDigest(token)
	link := Link{TokenHash: digest, CreatedBy: staff, ExpiresAt: expiresAt}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		return Entry{}, err
	}
	s.changed()
	entry := link.entry(time.Now())
	entry.Token = token
	return entry, nil
}

// List returns every link, canceled and expired ones included, oldest first.
// ponytail: unbounded retained history suits two staff and small mock data;
// paginate before sustained growth.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	var records []Link
	if err := s.db.WithContext(ctx).Order("created_at, token_hash").Find(&records).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	entries := make([]Entry, len(records))
	for i, link := range records {
		entries[i] = link.entry(now)
	}
	return entries, nil
}

// Lookup returns the usable link for token, submitted ones included, without
// changing it: it records no visit. Tokens that open nothing return nil.
func (s *Store) Lookup(ctx context.Context, token string) (*Link, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return nil, nil
	}
	var link Link
	result := s.active(ctx, digest).Limit(1).Find(&link)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &link, nil
}

// Open returns the usable link for token, recording the customer's first visit.
func (s *Store) Open(ctx context.Context, token string) (*Link, error) {
	link, err := s.Lookup(ctx, token)
	if err != nil || link == nil {
		return nil, err
	}
	if link.OpenedAt != nil {
		return link, nil
	}
	result := s.active(ctx, link.TokenHash).Model(link).Where("opened_at IS NULL").
		Clauses(clause.Returning{}).Update("opened_at", gorm.Expr("now()"))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return s.Lookup(ctx, token)
	}
	s.changed()
	return link, nil
}

// SaveDraft stores one validated field without finishing the form.
// Submitted forms never change: the customer's final answers are locked.
func (s *Store) SaveDraft(ctx context.Context, token, key, value string) (SaveResult, error) {
	field, ok := students.FindField(key)
	if !ok {
		return Unavailable, students.ErrUnknownField
	}
	digest, ok := tokenDigest(token)
	if !ok {
		return Unavailable, nil
	}
	result := s.active(ctx, digest).Where("submitted_at IS NULL").
		Where(clause.Neq{Column: clause.Column{Name: field.Key}, Value: value}).Update(field.Key, value)
	return s.saveResult(ctx, digest, result, Saved)
}

// Submit commits final answers and the PIN to the link, locking the form. A PIN held by an enrolled Student is refused here (PinTaken); one held by another submitted link is refused by idx_link_pin. No Student exists until Enroll.
// ponytail: a Submit racing a concurrent Enroll can still see the PIN free (READ COMMITTED); funding then fails on idx_student_pin. Upgrade: one PIN-reservation table or an advisory lock.
func (s *Store) Submit(ctx context.Context, token string, details students.Details, pin string) (SaveResult, error) {
	digest, ok := tokenDigest(token)
	if !ok {
		return Unavailable, nil
	}
	result := s.db.WithContext(ctx).Exec(`
UPDATE links
   SET name = ?, email = ?, phone = ?, municipality = ?, date_of_birth = ?, gender = ?, pin = ?,
       submitted_at = now(),
       expires_at = now() + interval '168 hours'
 WHERE token_hash = ?
   AND canceled_at IS NULL
   AND expires_at > now()
   AND submitted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM students WHERE pin = ? AND pin <> '')
`, details.Name, details.Email, details.Phone, details.Municipality, details.DateOfBirth, details.Gender, pin, digest, pin)
	if students.IsUniqueViolation(result.Error, pinIndex) {
		return PinTaken, nil
	}
	return s.saveResult(ctx, digest, result, PinTaken)
}

var errNotEnrollable = errors.New("links: link is not submitted or already enrolled")

// Enroll creates the link's Student at the moment payment is confirmed, inside the caller's transaction,
// which must already hold the Link row lock. The Student takes the link's contact snapshot, its PIN
// (the Student's unique index owns it from here, so the link's copy is cleared) and paidAt as created_at.
// Its cohort is the month of the agreement's start date, which checkout requires; its group is the
// column default, Básico.
// The link keeps its snapshot. It never checks Usable: money was collected, so an expired link still enrolls.
func Enroll(tx *gorm.DB, hash []byte, paidAt time.Time) error {
	result := tx.Exec(`
INSERT INTO students (source_link_hash, name, email, phone, municipality, date_of_birth, gender, pin, cohort, created_at)
SELECT token_hash, name, email, phone, municipality, date_of_birth, gender, pin, left(program_start_date, 7), ?
  FROM links WHERE token_hash = ? AND submitted_at IS NOT NULL AND paid_at IS NULL`, paidAt, hash)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errNotEnrollable // impossible for a checkout-gated contract: fail the funding transaction loudly
	}
	return tx.Model(&Link{}).Where("token_hash = ?", hash).Updates(map[string]any{"paid_at": paidAt, "pin": ""}).Error
}

// AcceptTerms snapshots the displayed start date once, after contact submission.
// Accepted replays preserve the original agreement even when settings change.
func (s *Store) AcceptTerms(ctx context.Context, token, shownStartDate string) (*Link, error) {
	link, err := s.Lookup(ctx, token)
	if err != nil || link == nil || link.SubmittedAt == nil {
		return nil, err
	}
	if link.TermsAcceptedAt != nil {
		return link, nil
	}
	startDate, err := settings.ParseProgramStartDate(shownStartDate)
	if err != nil {
		return nil, ErrProgramStartChanged
	}
	var accepted Link
	// The settings row lock serializes acceptance with staff date changes.
	result := s.active(ctx, link.TokenHash).Model(&accepted).
		Where("submitted_at IS NOT NULL AND terms_accepted_at IS NULL").
		Where("? = (SELECT program_start_date FROM settings WHERE id = 1 FOR SHARE)", startDate).
		Clauses(clause.Returning{}).Updates(map[string]any{
		"terms_accepted_at": gorm.Expr("now()"), "program_start_date": startDate,
	})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected > 0 {
		s.changed()
		return &accepted, nil
	}
	link, err = s.Lookup(ctx, token)
	if err != nil || link == nil || link.SubmittedAt == nil {
		return nil, err
	}
	if link.TermsAcceptedAt != nil {
		return link, nil
	}
	return nil, ErrProgramStartChanged
}

func (s *Store) saveResult(ctx context.Context, digest []byte, result *gorm.DB, unfinishedResult SaveResult) (SaveResult, error) {
	if result.Error != nil {
		return Unavailable, result.Error
	}
	if result.RowsAffected == 1 {
		s.changed()
		return Saved, nil
	}
	var link Link
	result = s.active(ctx, digest).Select("submitted_at").Limit(1).Find(&link)
	if result.Error != nil {
		return Unavailable, result.Error
	}
	if result.RowsAffected == 0 {
		return Unavailable, nil
	}
	if link.SubmittedAt != nil {
		return AlreadySubmitted, nil
	}
	return unfinishedResult, nil
}

// Cancel revokes an unfinished link whose Entry.ID is id and returns its
// persisted entry without a token. Finished, expired and previously canceled
// links stay unchanged, preserving the first cancellation's audit.
// Malformed and unknown IDs return nil.
func (s *Store) Cancel(ctx context.Context, id, staff string) (*Entry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	digest, err := hex.DecodeString(id)
	if err != nil || len(digest) != sha256.Size {
		return nil, nil
	}
	var link Link
	result := s.active(ctx, digest).Model(&link).Where("submitted_at IS NULL").
		Clauses(clause.Returning{}).Updates(canceledBy(staff))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		link = Link{}
		result = s.db.WithContext(ctx).Where(&Link{TokenHash: digest}).Limit(1).Find(&link)
		if result.Error != nil || result.RowsAffected == 0 {
			return nil, result.Error
		}
	} else {
		s.changed()
	}
	entry := link.entry(time.Now())
	return &entry, nil
}

// CancelAll revokes usable unfinished links on behalf of staff.
// Finished, expired and previously canceled links stay unchanged.
func (s *Store) CancelAll(ctx context.Context, staff string) error {
	if staff == "" {
		return errNoStaff
	}
	result := s.usable(ctx).Where("submitted_at IS NULL").Updates(canceledBy(staff))
	if result.RowsAffected > 0 {
		s.changed()
	}
	return result.Error
}

func canceledBy(staff string) map[string]any {
	return map[string]any{"canceled_at": gorm.Expr("now()"), "canceled_by": staff}
}

// active scopes a query to the usable link with digest.
func (s *Store) active(ctx context.Context, digest []byte) *gorm.DB {
	return s.usable(ctx).Where(&Link{TokenHash: digest})
}

// Usable scopes a query to tokens that still open the enrollment flow:
// neither canceled nor past their access deadline.
func Usable(db *gorm.DB) *gorm.DB {
	return db.Where("canceled_at IS NULL AND expires_at > now()")
}

func (s *Store) usable(ctx context.Context) *gorm.DB {
	return s.db.WithContext(ctx).Model(&Link{}).Scopes(Usable)
}

// tokenDigest rejects tokens of the wrong length before hashing.
func tokenDigest(token string) ([]byte, bool) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], true
}
