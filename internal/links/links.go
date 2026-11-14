// Package links stores reusable customer links and cookie-authenticated customer enrollment sessions,
// owning their life from staff issuance through drafts, final submit, agreement acceptance and cancellation.
// Enroll, called inside billing's funding transaction, turns a paid enrollment into a Student. See docs/domain/links.md.
package links

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"projecthermes/internal/payments"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
)

// pinIndex names Enrollment.Pin's unique index; the struct tag below cannot reference it.
const pinIndex = "idx_enrollment_pin"

const tokenBytes = 32

// MaxLinkEnrollments caps how many enrollments one reusable link can accrue across any status.
const MaxLinkEnrollments = 100

// MaxPinChecks bounds the PIN availability answers one enrollment can obtain, so a holder cannot enumerate the PINs students hold.
const MaxPinChecks = 30

// Link is a reusable, named customer URL (/customer/{token}) that staff share with customers.
// The token opens only the welcome gate and never exposes customer PII, so storing it allows copying anytime.
type Link struct {
	ID          uint64       `gorm:"primaryKey"`
	Name        string       `gorm:"not null;default:''"`
	Token       string       `gorm:"not null;uniqueIndex;check:link_token_length,char_length(token) = 43"`
	CreatedAt   time.Time    `gorm:"not null;default:CURRENT_TIMESTAMP"`
	ExpiresAt   time.Time    `gorm:"not null;default:CURRENT_TIMESTAMP"`
	CanceledAt  *time.Time
	CreatedBy   string       `gorm:"not null;default:''"`
	CanceledBy  string       `gorm:"not null;default:''"`
	Enrollments []Enrollment `gorm:"foreignKey:LinkID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

// Enrollment stores one customer's enrollment session by SHA-256 hash of its cookie token.
type Enrollment struct {
	ID          uint64 `gorm:"primaryKey"`
	LinkID      uint64 `gorm:"not null;index"`
	Link        *Link  `gorm:"foreignKey:LinkID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
	SessionHash []byte `gorm:"not null;uniqueIndex;check:session_hash_size,octet_length(session_hash) = 32"`
	students.Details
	// Pin is the PIN chosen at submit. It reserves the PIN until payment moves it to the Student; "" before submit and after payment.
	// The index name must equal pinIndex.
	Pin       string `gorm:"not null;default:'';uniqueIndex:idx_enrollment_pin,where:pin <> ''"`
	PinChecks int    `gorm:"not null;default:0"`
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	// SubmittedAt is the customer's final submit; autosave never sets it.
	SubmittedAt *time.Time
	// TermsAcceptedAt and ProgramStartDate retain the agreement first accepted.
	TermsAcceptedAt  *time.Time `gorm:"type:timestamptz"`
	ProgramStartDate string     `gorm:"type:text;not null;default:''"`
	// PaidAt is when the first payment was confirmed: the instant Enroll created the Student. Nil while unpaid.
	PaidAt *time.Time `gorm:"type:timestamptz"`
	// ExpiresAt starts at the parent Link's expiry; Submit replaces it with 168 hours from submission.
	ExpiresAt  time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	CanceledAt *time.Time
	CanceledBy string            `gorm:"not null;default:''"`
	Student    *students.Student `gorm:"foreignKey:EnrollmentID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

// LinkStatus is a reusable link's status as shown to staff.
type LinkStatus string

const (
	LinkActive   LinkStatus = "Active"
	LinkExpired  LinkStatus = "Expired"
	LinkCanceled LinkStatus = "Canceled"
)

// Status is an enrollment's customer progress as shown to staff.
type Status string

const (
	// InProgress means an enrollment session started and the customer hasn't submitted yet.
	InProgress Status = "In progress"
	// AwaitingPayment means the customer submitted the form; no payment confirmed yet.
	AwaitingPayment Status = "Awaiting payment"
	// Finished means payment confirmed and the Student created.
	Finished Status = "Finished"
	// Expired means the enrollment lapsed before payment was confirmed.
	Expired  Status = "Expired"
	Canceled Status = "Canceled"
)

// SaveResult reports the authoritative outcome of a customer write.
type SaveResult uint8

const (
	Unavailable SaveResult = iota
	Saved
	AlreadySubmitted
	// PinTaken means another submitted enrollment or enrolled student holds the PIN; the form stays open.
	PinTaken
)

// LinkEntry is a reusable link as shown to staff.
type LinkEntry struct {
	ID              uint64
	Name            string
	Token           string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	CreatedBy       string
	CanceledBy      string
	Status          LinkStatus
	EnrollmentCount int
}

// Cancelable reports whether staff can still revoke an active link.
func (e LinkEntry) Cancelable() bool {
	return e.Status == LinkActive
}

// Trashed reports whether the link belongs in canceled or expired history.
func (e LinkEntry) Trashed() bool {
	return e.Status == LinkCanceled || e.Status == LinkExpired
}

// EnrollmentEntry is a customer enrollment as shown to staff under its parent link.
type EnrollmentEntry struct {
	ID               uint64
	LinkID           uint64
	Name             string
	Phone            string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	SubmittedAt      *time.Time
	TermsAcceptedAt  *time.Time
	ProgramStartDate string
	PaidAt           *time.Time
	CanceledAt       *time.Time
	CanceledBy       string
	Status           Status
	HasPin           bool
}

// Cancelable reports whether staff can revoke the enrollment or clear an expired submission's PIN.
func (e EnrollmentEntry) Cancelable() bool {
	return e.Status == InProgress || e.Status == AwaitingPayment || (e.Status == Expired && e.HasPin)
}

// Archived reports whether payment is at least 24 hours old at now.
func (e EnrollmentEntry) Archived(now time.Time) bool {
	return e.Status == Finished && e.PaidAt != nil && !e.PaidAt.After(now.Add(-24*time.Hour))
}

// Trashed reports whether the enrollment belongs in canceled or expired history.
func (e EnrollmentEntry) Trashed() bool {
	return e.Status == Canceled || e.Status == Expired
}

// LinkGroup pairs a reusable link with its customer enrollments.
type LinkGroup struct {
	Link        LinkEntry
	Enrollments []EnrollmentEntry
}

// Session is the resolved state of a customer's request against a reusable link and optional cookie.
type Session struct {
	Link             Link
	LinkUsable       bool
	Enrollment       *Enrollment
	EnrollmentStatus Status
	Usable           bool
}

func (l Link) status(now time.Time) LinkStatus {
	switch {
	case l.CanceledAt != nil:
		return LinkCanceled
	case !now.Before(l.ExpiresAt):
		return LinkExpired
	default:
		return LinkActive
	}
}

func (l Link) entry(now time.Time, count int) LinkEntry {
	return LinkEntry{
		ID: l.ID, Name: l.Name, Token: l.Token,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt,
		CreatedBy: l.CreatedBy, CanceledBy: l.CanceledBy,
		Status: l.status(now), EnrollmentCount: count,
	}
}

func (e Enrollment) status(link Link, now time.Time) (Status, string) {
	switch {
	case e.CanceledAt != nil:
		return Canceled, e.CanceledBy
	case e.PaidAt != nil:
		return Finished, ""
	case e.SubmittedAt != nil:
		if now.Before(e.ExpiresAt) {
			return AwaitingPayment, ""
		}
		return Expired, ""
	case link.CanceledAt != nil:
		return Canceled, link.CanceledBy
	case !now.Before(link.ExpiresAt) || !now.Before(e.ExpiresAt):
		return Expired, ""
	default:
		return InProgress, ""
	}
}

func (e Enrollment) entry(link Link, now time.Time) EnrollmentEntry {
	status, canceledBy := e.status(link, now)
	return EnrollmentEntry{
		ID: e.ID, LinkID: e.LinkID,
		Name: e.Name, Phone: e.Phone,
		CreatedAt: e.CreatedAt, ExpiresAt: e.ExpiresAt,
		SubmittedAt: e.SubmittedAt, TermsAcceptedAt: e.TermsAcceptedAt, ProgramStartDate: e.ProgramStartDate,
		PaidAt: e.PaidAt, CanceledAt: e.CanceledAt, CanceledBy: canceledBy,
		Status: status, HasPin: e.Pin != "",
	}
}

// errNoStaff rejects staff actions that would leave an anonymous audit entry.
var errNoStaff = errors.New("links: staff username required")

// ErrProgramStartChanged requires the customer to review the current start date.
var ErrProgramStartChanged = errors.New("links: program start date changed or unavailable")

// ErrInvalidLinkName rejects empty, overlong, or control-character link names.
var ErrInvalidLinkName = errors.New("links: invalid link name")
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

// CreateLink issues a reusable link that stops accepting new starts at expiresAt.
// Empty names default to today's Puerto Rico date (YYYY-MM-DD).
func (s *Store) CreateLink(ctx context.Context, staff, name string, expiresAt time.Time) (LinkEntry, error) {
	if staff == "" {
		return LinkEntry{}, errNoStaff
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = time.Now().In(payments.PuertoRico).Format(time.DateOnly)
	}
	token := randomToken()
	link := Link{Name: name, Token: token, CreatedBy: staff, ExpiresAt: expiresAt}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		return LinkEntry{}, err
	}
	s.changed()
	return link.entry(time.Now(), 0), nil
}

// ListGroups returns every reusable link with its child enrollments, oldest first.
// ponytail: unbounded retained history suits two staff and small mock data;
// paginate before sustained growth.
func (s *Store) ListGroups(ctx context.Context) ([]LinkGroup, error) {
	var allLinks []Link
	if err := s.db.WithContext(ctx).Order("created_at, id").Find(&allLinks).Error; err != nil {
		return nil, err
	}
	var allEnrollments []Enrollment
	if err := s.db.WithContext(ctx).Order("created_at, id").Find(&allEnrollments).Error; err != nil {
		return nil, err
	}
	byLink := make(map[uint64][]Enrollment, len(allLinks))
	for _, e := range allEnrollments {
		byLink[e.LinkID] = append(byLink[e.LinkID], e)
	}
	now := time.Now()
	groups := make([]LinkGroup, len(allLinks))
	for i, link := range allLinks {
		children := byLink[link.ID]
		entries := make([]EnrollmentEntry, len(children))
		for j, child := range children {
			entries[j] = child.entry(link, now)
		}
		groups[i] = LinkGroup{
			Link:        link.entry(now, len(children)),
			Enrollments: entries,
		}
	}
	return groups, nil
}

// LookupLink returns the Link row matching token (regardless of expiry/cancellation), or nil if unknown or malformed.
func (s *Store) LookupLink(ctx context.Context, token string) (*Link, error) {
	if !validToken(token) {
		return nil, nil
	}
	var link Link
	result := s.db.WithContext(ctx).Where("token = ?", token).Limit(1).Find(&link)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, result.Error
	}
	return &link, nil
}

// LookupSession resolves a customer's reusable link and optional cookie session without writing to the database.
// Unknown or malformed link tokens return nil.
func (s *Store) LookupSession(ctx context.Context, linkToken, cookieToken string) (*Session, error) {
	link, err := s.LookupLink(ctx, linkToken)
	if err != nil || link == nil {
		return nil, err
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&Enrollment{}).Where("link_id = ?", link.ID).Count(&count).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		Link:       *link,
		LinkUsable: link.CanceledAt == nil && now.Before(link.ExpiresAt) && count < MaxLinkEnrollments,
	}
	if digest, ok := tokenDigest(cookieToken); ok {
		var e Enrollment
		result := s.db.WithContext(ctx).Where("link_id = ? AND session_hash = ?", link.ID, digest).Limit(1).Find(&e)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			status, _ := e.status(*link, now)
			sess.Enrollment = &e
			sess.EnrollmentStatus = status
			if e.SubmittedAt == nil {
				sess.Usable = e.CanceledAt == nil && now.Before(e.ExpiresAt) && link.CanceledAt == nil && now.Before(link.ExpiresAt)
			} else {
				// expires_at is the deadline to pay; once paid it no longer limits reading.
				sess.Usable = e.CanceledAt == nil && (e.PaidAt != nil || now.Before(e.ExpiresAt))
			}
		}
	}
	return sess, nil
}

// StartEnrollment returns the browser's live unpaid enrollment on linkToken if one exists,
// or creates a new Enrollment under MaxLinkEnrollments and returns its fresh cookie token.
// Unusable or full links with no live unpaid enrollment return (nil, "", nil).
func (s *Store) StartEnrollment(ctx context.Context, linkToken, existingCookieToken string) (*Enrollment, string, error) {
	if !validToken(linkToken) {
		return nil, "", nil
	}
	var result *Enrollment
	var newCookie string
	var createdNew bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link Link
		res := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
			Where("token = ?", linkToken).Limit(1).Find(&link)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		now := time.Now()
		if digest, ok := tokenDigest(existingCookieToken); ok {
			var existing Enrollment
			res := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
				Where("link_id = ? AND session_hash = ?", link.ID, digest).Limit(1).Find(&existing)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 1 {
				status, _ := existing.status(link, now)
				if status == InProgress || status == AwaitingPayment {
					result = &existing
					return nil
				}
			}
		}
		if link.CanceledAt != nil || !now.Before(link.ExpiresAt) {
			return nil
		}
		var count int64
		if err := tx.Model(&Enrollment{}).Where("link_id = ?", link.ID).Count(&count).Error; err != nil {
			return err
		}
		if count >= MaxLinkEnrollments {
			return nil
		}
		newCookie = randomToken()
		digest, _ := tokenDigest(newCookie)
		created := Enrollment{
			LinkID:      link.ID,
			SessionHash: digest,
			ExpiresAt:   link.ExpiresAt,
		}
		if err := tx.Create(&created).Error; err != nil {
			return err
		}
		result = &created
		createdNew = true
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if createdNew {
		s.changed()
	}
	return result, newCookie, nil
}

// SaveDraft stores one validated field without finishing the form.
// Submitted forms never change: the customer's final answers are locked.
func (s *Store) SaveDraft(ctx context.Context, linkToken, cookieToken, key, value string) (SaveResult, error) {
	field, ok := students.FindField(key)
	if !ok {
		return Unavailable, students.ErrUnknownField
	}
	if !validToken(linkToken) {
		return Unavailable, nil
	}
	digest, ok := tokenDigest(cookieToken)
	if !ok {
		return Unavailable, nil
	}
	result := s.activeUnsubmitted(ctx, linkToken, digest).Where("submitted_at IS NULL").
		Where(clause.Neq{Column: clause.Column{Name: field.Key}, Value: value}).Update(field.Key, value)
	return s.saveResult(ctx, linkToken, digest, result, Saved)
}

// Submit commits final answers and the PIN to the enrollment, locking the form. A PIN held by an enrolled Student is refused here (PinTaken); one held by another submitted enrollment is refused by idx_enrollment_pin. No Student exists until Enroll.
// ponytail: a Submit racing a concurrent Enroll can still see the PIN free (READ COMMITTED); funding then fails on idx_student_pin. Upgrade: one PIN-reservation table or an advisory lock.
func (s *Store) Submit(ctx context.Context, linkToken, cookieToken string, details students.Details, pin string) (SaveResult, error) {
	if !validToken(linkToken) {
		return Unavailable, nil
	}
	digest, ok := tokenDigest(cookieToken)
	if !ok {
		return Unavailable, nil
	}
	result := s.db.WithContext(ctx).Exec(`
UPDATE enrollments e
   SET name = ?, email = ?, phone = ?, municipality = ?, date_of_birth = ?, gender = ?, pin = ?,
       submitted_at = now(),
       expires_at = now() + interval '168 hours'
  FROM links l
 WHERE e.link_id = l.id
   AND l.token = ?
   AND l.canceled_at IS NULL
   AND l.expires_at > now()
   AND e.session_hash = ?
   AND e.canceled_at IS NULL
   AND e.expires_at > now()
   AND e.submitted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM students WHERE pin = ? AND pin <> '')
`, details.Name, details.Email, details.Phone, details.Municipality, details.DateOfBirth, details.Gender, pin, linkToken, digest, pin)
	if students.IsUniqueViolation(result.Error, pinIndex) {
		return PinTaken, nil
	}
	return s.saveResult(ctx, linkToken, digest, result, PinTaken)
}

// SpendPinCheck counts one PIN availability answer against the usable, unsubmitted enrollment
// behind (linkToken, cookieToken) and reports whether the enrollment still had budget.
func (s *Store) SpendPinCheck(ctx context.Context, linkToken, cookieToken string) (bool, error) {
	if !validToken(linkToken) {
		return false, nil
	}
	digest, ok := tokenDigest(cookieToken)
	if !ok {
		return false, nil
	}
	result := s.activeUnsubmitted(ctx, linkToken, digest).Where("submitted_at IS NULL AND pin_checks < ?", MaxPinChecks).
		Update("pin_checks", gorm.Expr("pin_checks + 1"))
	return result.RowsAffected == 1, result.Error
}

var errNotEnrollable = errors.New("links: enrollment is not submitted or already enrolled")

// Enroll creates the enrollment's Student at the moment payment is confirmed, inside the caller's transaction,
// which must already hold the Enrollment row lock. The Student takes the enrollment's contact snapshot, its PIN
// (the Student's unique index owns it from here, so the enrollment's copy is cleared) and paidAt as created_at.
// Its cohort is the month of the agreement's start date, which checkout requires; its group is the
// column default, Básico.
// The enrollment keeps its snapshot. It never checks Usable: money was collected, so an expired enrollment still enrolls.
func Enroll(tx *gorm.DB, enrollmentID uint64, paidAt time.Time) error {
	result := tx.Exec(`
INSERT INTO students (enrollment_id, name, email, phone, municipality, date_of_birth, gender, pin, cohort, created_at)
SELECT id, name, email, phone, municipality, date_of_birth, gender, pin, left(program_start_date, 7), ?
  FROM enrollments WHERE id = ? AND submitted_at IS NOT NULL AND paid_at IS NULL`, paidAt, enrollmentID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errNotEnrollable // impossible for a checkout-gated contract: fail the funding transaction loudly
	}
	return tx.Model(&Enrollment{}).Where("id = ?", enrollmentID).Updates(map[string]any{"paid_at": paidAt, "pin": ""}).Error
}

// AcceptTerms snapshots the displayed start date once, after contact submission.
// Accepted replays preserve the original agreement even when settings change.
// Once submitted, the enrollment relies on its own expiry and cancellation rather than the parent link's.
func (s *Store) AcceptTerms(ctx context.Context, linkToken, cookieToken, shownStartDate string) (*Enrollment, error) {
	if !validToken(linkToken) {
		return nil, nil
	}
	digest, ok := tokenDigest(cookieToken)
	if !ok {
		return nil, nil
	}
	lookupSubmitted := func() (*Enrollment, error) {
		var e Enrollment
		res := s.activeSubmitted(ctx, linkToken, digest).Limit(1).Find(&e)
		if res.Error != nil || res.RowsAffected == 0 {
			return nil, res.Error
		}
		return &e, nil
	}
	e, err := lookupSubmitted()
	if err != nil || e == nil {
		return nil, err
	}
	if e.TermsAcceptedAt != nil {
		return e, nil
	}
	startDate, err := settings.ParseProgramStartDate(shownStartDate)
	if err != nil {
		return nil, ErrProgramStartChanged
	}
	var accepted Enrollment
	// The settings row lock serializes acceptance with staff date changes.
	result := s.activeSubmitted(ctx, linkToken, digest).Model(&accepted).
		Where("terms_accepted_at IS NULL").
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
	e, err = lookupSubmitted()
	if err != nil || e == nil {
		return nil, err
	}
	if e.TermsAcceptedAt != nil {
		return e, nil
	}
	return nil, ErrProgramStartChanged
}

func (s *Store) saveResult(ctx context.Context, linkToken string, digest []byte, result *gorm.DB, unfinishedResult SaveResult) (SaveResult, error) {
	if result.Error != nil {
		return Unavailable, result.Error
	}
	if result.RowsAffected == 1 {
		s.changed()
		return Saved, nil
	}
	var e Enrollment
	res := s.activeUnsubmitted(ctx, linkToken, digest).Select("submitted_at").Limit(1).Find(&e)
	if res.Error != nil {
		return Unavailable, res.Error
	}
	if res.RowsAffected == 1 {
		if e.SubmittedAt != nil {
			return AlreadySubmitted, nil
		}
		return unfinishedResult, nil
	}
	// A submitted enrollment stays usable for AlreadySubmitted even if its parent link later lapsed or canceled.
	res = s.activeSubmitted(ctx, linkToken, digest).Select("submitted_at").Limit(1).Find(&e)
	if res.Error != nil {
		return Unavailable, res.Error
	}
	if res.RowsAffected == 1 {
		return AlreadySubmitted, nil
	}
	return Unavailable, nil
}

// CancelLink revokes an active link so new visitors can no longer start enrollments on it.
// Unsubmitted enrollments under it derive Canceled; already-submitted enrollments keep their 168h window.
func (s *Store) CancelLink(ctx context.Context, id uint64, staff string) (*LinkEntry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	if !validID(id) {
		return nil, nil
	}
	var link Link
	result := s.db.WithContext(ctx).Model(&link).Scopes(Usable).Where("id = ?", id).
		Clauses(clause.Returning{}).Updates(canceledBy(staff))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		link = Link{}
		result = s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&link)
		if result.Error != nil || result.RowsAffected == 0 {
			return nil, result.Error
		}
	} else {
		s.changed()
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&Enrollment{}).Where("link_id = ?", link.ID).Count(&count).Error; err != nil {
		return nil, err
	}
	entry := link.entry(time.Now(), int(count))
	return &entry, nil
}

const maxLinkNameRunes = 100

// RenameLink updates a reusable link's display name on behalf of staff.
// Unknown or out-of-range IDs return (nil, nil).
func (s *Store) RenameLink(ctx context.Context, id uint64, name, staff string) (*LinkEntry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxLinkNameRunes {
		return nil, ErrInvalidLinkName
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return nil, ErrInvalidLinkName
		}
	}
	if !validID(id) {
		return nil, nil
	}
	var link Link
	upd := s.db.WithContext(ctx).Model(&link).Where("id = ? AND name <> ?", id, name).
		Clauses(clause.Returning{}).Update("name", name)
	if upd.Error != nil {
		return nil, upd.Error
	}
	if upd.RowsAffected > 0 {
		s.changed()
	} else {
		res := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&link)
		if res.Error != nil || res.RowsAffected == 0 {
			return nil, res.Error
		}
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&Enrollment{}).Where("link_id = ?", link.ID).Count(&count).Error; err != nil {
		return nil, err
	}
	entry := link.entry(time.Now(), int(count))
	return &entry, nil
}

// CancelEnrollment revokes an unsubmitted, active enrollment on behalf of staff.
// Submitted, finished, expired and previously canceled enrollments stay unchanged.
// For billing-safe cancellation of submitted enrollments or expired PINs, use billing.Service.CancelEnrollment.
func (s *Store) CancelEnrollment(ctx context.Context, id uint64, staff string) (*EnrollmentEntry, error) {
	if staff == "" {
		return nil, errNoStaff
	}
	if !validID(id) {
		return nil, nil
	}
	var e Enrollment
	result := s.usable(ctx).Model(&e).
		Where("id = ? AND submitted_at IS NULL AND EXISTS (SELECT 1 FROM links WHERE links.id = enrollments.link_id AND links.canceled_at IS NULL AND links.expires_at > now())", id).
		Clauses(clause.Returning{}).Updates(canceledBy(staff))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		e = Enrollment{}
		result = s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&e)
		if result.Error != nil || result.RowsAffected == 0 {
			return nil, result.Error
		}
	} else {
		s.changed()
	}
	var link Link
	if err := s.db.WithContext(ctx).Where("id = ?", e.LinkID).Take(&link).Error; err != nil {
		return nil, err
	}
	entry := e.entry(link, time.Now())
	return &entry, nil
}

// CancelEnrollmentTx cancels an InProgress or AwaitingPayment enrollment (clearing its PIN),
// or clears an Expired submission's PIN without turning it into Canceled.
// The caller must already hold the Enrollment row lock (and, when submitted, have verified billing safety).
func CancelEnrollmentTx(tx *gorm.DB, id uint64, staff string) (entry *EnrollmentEntry, changed bool, err error) {
	if staff == "" {
		return nil, false, errNoStaff
	}
	var e Enrollment
	res := tx.Where("id = ?", id).Limit(1).Find(&e)
	if res.Error != nil || res.RowsAffected == 0 {
		return nil, false, res.Error
	}
	var link Link
	if err := tx.Where("id = ?", e.LinkID).Take(&link).Error; err != nil {
		return nil, false, err
	}
	now := time.Now()
	status, _ := e.status(link, now)
	switch status {
	case InProgress:
		upd := tx.Model(&e).Where("id = ? AND canceled_at IS NULL AND submitted_at IS NULL", id).
			Clauses(clause.Returning{}).Updates(canceledBy(staff))
		if upd.Error != nil {
			return nil, false, upd.Error
		}
		changed = upd.RowsAffected > 0
	case AwaitingPayment:
		updates := canceledBy(staff)
		updates["pin"] = ""
		upd := tx.Model(&e).Where("id = ? AND canceled_at IS NULL AND paid_at IS NULL", id).
			Clauses(clause.Returning{}).Updates(updates)
		if upd.Error != nil {
			return nil, false, upd.Error
		}
		changed = upd.RowsAffected > 0
	case Expired:
		if e.Pin != "" {
			upd := tx.Model(&e).Where("id = ? AND paid_at IS NULL AND pin <> ''", id).
				Clauses(clause.Returning{}).Update("pin", "")
			if upd.Error != nil {
				return nil, false, upd.Error
			}
			changed = upd.RowsAffected > 0
		}
	}
	out := e.entry(link, now)
	return &out, changed, nil
}
func canceledBy(staff string) map[string]any {
	return map[string]any{"canceled_at": gorm.Expr("now()"), "canceled_by": staff}
}

func validID(id uint64) bool {
	return id > 0 && id <= 1<<63-1
}

func (s *Store) activeUnsubmitted(ctx context.Context, linkToken string, digest []byte) *gorm.DB {
	return s.usable(ctx).Where(
		"session_hash = ? AND EXISTS (SELECT 1 FROM links WHERE links.id = enrollments.link_id AND links.token = ? AND links.canceled_at IS NULL AND links.expires_at > now())",
		digest, linkToken,
	)
}

func (s *Store) activeSubmitted(ctx context.Context, linkToken string, digest []byte) *gorm.DB {
	return s.usable(ctx).Where(
		"submitted_at IS NOT NULL AND session_hash = ? AND EXISTS (SELECT 1 FROM links WHERE links.id = enrollments.link_id AND links.token = ?)",
		digest, linkToken,
	)
}

// Usable scopes a query to rows that are neither canceled nor past their access deadline.
func Usable(db *gorm.DB) *gorm.DB {
	return db.Where("canceled_at IS NULL AND expires_at > now()")
}

func (s *Store) usable(ctx context.Context) *gorm.DB {
	return s.db.WithContext(ctx).Model(&Enrollment{}).Scopes(Usable)
}

func randomToken() string {
	var random [tokenBytes]byte
	// Go 1.26 terminates the process if the OS random source fails.
	rand.Read(random[:])
	return base64.RawURLEncoding.EncodeToString(random[:])
}

func validToken(token string) bool {
	return len(token) == base64.RawURLEncoding.EncodedLen(tokenBytes)
}

// tokenDigest rejects tokens of the wrong length before hashing.
func tokenDigest(token string) ([]byte, bool) {
	if !validToken(token) {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], true
}
