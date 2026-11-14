// Package links stores reusable customer links and cookie-authenticated customer enrollment sessions,
// owning their life from staff issuance through drafts, final submit, agreement acceptance and cancellation.
// Enroll, called inside billing's funding transaction, turns a paid enrollment into a Student. See docs/domain/links.md.
package links

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"projecthermes/internal/puertorico"
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
	ID         uint64    `gorm:"primaryKey"`
	Name       string    `gorm:"not null;default:''"`
	Token      string    `gorm:"not null;uniqueIndex;check:link_token_length,char_length(token) = 43"`
	CreatedAt  time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	ExpiresAt  time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
	CanceledAt *time.Time
	CreatedBy  string `gorm:"not null;default:''"`
	CanceledBy string `gorm:"not null;default:''"`
	// EnrollmentCount is the enrollments ever started; enrollments are never deleted, and the only INSERT is StartEnrollment's.
	EnrollmentCount int          `gorm:"not null;default:0"`
	Enrollments     []Enrollment `gorm:"foreignKey:LinkID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

// Enrollment stores one customer's enrollment session by SHA-256 hash of its cookie token.
type Enrollment struct {
	ID          uint64 `gorm:"primaryKey"`
	LinkID      uint64 `gorm:"not null;index"`
	SessionHash []byte `gorm:"not null;uniqueIndex;check:session_hash_size,octet_length(session_hash) = 32"`
	students.Details
	// Pin is the PIN chosen at submit. It reserves the PIN until payment moves it to the Student; "" before submit and after payment.
	// The index name must equal pinIndex.
	Pin       string    `gorm:"not null;default:'';uniqueIndex:idx_enrollment_pin,where:pin <> ''"`
	PinChecks int       `gorm:"not null;default:0"`
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

// SaveResult reports the authoritative state of a customer's form after a write or a FormState probe.
type SaveResult uint8

const (
	Unavailable SaveResult = iota
	Saved
	AlreadySubmitted
	// PinTaken means another submitted enrollment or enrolled student holds the PIN; the form stays open.
	PinTaken
	// PinLimited means the open form has no PIN checks left, so Submit wrote nothing.
	PinLimited
	// Open means the form is usable and unsubmitted and nothing was written; only FormState returns it.
	Open
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
	Name             string
	Phone            string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	SubmittedAt      *time.Time
	TermsAcceptedAt  *time.Time
	ProgramStartDate string
	PaidAt           *time.Time
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

func (l Link) entry(now time.Time) LinkEntry {
	return LinkEntry{
		ID: l.ID, Name: l.Name, Token: l.Token,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt,
		CreatedBy: l.CreatedBy, CanceledBy: l.CanceledBy,
		Status: l.status(now), EnrollmentCount: l.EnrollmentCount,
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
		ID:   e.ID,
		Name: e.Name, Phone: e.Phone,
		CreatedAt: e.CreatedAt, ExpiresAt: e.ExpiresAt,
		SubmittedAt: e.SubmittedAt, TermsAcceptedAt: e.TermsAcceptedAt, ProgramStartDate: e.ProgramStartDate,
		PaidAt: e.PaidAt, CanceledBy: canceledBy,
		Status: status, HasPin: e.Pin != "",
	}
}

// errNoStaff rejects staff actions that would leave an anonymous audit entry.
var errNoStaff = errors.New("links: staff username required")

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
		name = time.Now().In(puertorico.Zone).Format(time.DateOnly)
	}
	token := randomToken()
	link := Link{Name: name, Token: token, CreatedBy: staff, ExpiresAt: expiresAt}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		return LinkEntry{}, err
	}
	s.changed()
	return link.entry(time.Now()), nil
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
			Link:        link.entry(now),
			Enrollments: entries,
		}
	}
	return groups, nil
}

// sessionRow is LookupSession's result: whether the link takes new enrollments, and the cookie's usable
// enrollment, whose columns are all NULL (ID 0) when there is none.
type sessionRow struct {
	Enrollment
	CanStart bool
}

// LookupSession resolves, in one read, the usable enrollment behind a customer's session cookie on a reusable link
// (nil when there is none) and whether the link takes new enrollments now. Unknown or malformed link tokens
// return (nil, false, nil); a malformed cookie's nil digest matches no enrollment.
func (s *Store) LookupSession(ctx context.Context, linkToken, cookieToken string) (e *Enrollment, canStart bool, err error) {
	if !validToken(linkToken) {
		return nil, false, nil
	}
	digest, _ := tokenDigest(cookieToken)
	var row sessionRow
	err = s.db.WithContext(ctx).Raw(`
SELECT l.canceled_at IS NULL AND l.expires_at > now() AND l.enrollment_count < ? AS can_start, e.*
  FROM links l LEFT JOIN enrollments e ON `+usableSession+`
 WHERE l.token = ?`, MaxLinkEnrollments, linkToken, digest, linkToken).Scan(&row).Error
	if err != nil || row.ID == 0 {
		return nil, row.CanStart, err
	}
	return &row.Enrollment, row.CanStart, nil
}

// StartEnrollment returns the browser's live unpaid enrollment on linkToken if one exists,
// or creates a new Enrollment under MaxLinkEnrollments and returns its fresh cookie token.
// Unusable or full links with no live unpaid enrollment return (nil, "", nil).
func (s *Store) StartEnrollment(ctx context.Context, linkToken, existingCookieToken string) (*Enrollment, string, error) {
	if !validToken(linkToken) {
		return nil, "", nil
	}
	if _, ok := tokenDigest(existingCookieToken); ok {
		e, _, err := s.LookupSession(ctx, linkToken, existingCookieToken)
		if err != nil || (e != nil && e.PaidAt == nil) {
			return e, "", err
		}
	}
	cookie := randomToken()
	digest, _ := tokenDigest(cookie)
	var created Enrollment
	// ponytail: the cap holds because concurrent starts serialize on the link row and READ COMMITTED rechecks
	// enrollment_count < ? on its newest version; a count(*) in the same statement would read a stale snapshot.
	// The statement commits with synchronous_commit off, so a PostgreSQL crash can lose enrollments started in
	// roughly the last 600 ms and the customer starts again. Drop the async CTE to make starts durable.
	result := s.db.WithContext(ctx).Raw(`
WITH async AS (SELECT set_config('synchronous_commit', 'off', true)),
slot AS (UPDATE links l SET enrollment_count = l.enrollment_count + 1
           FROM async
          WHERE l.token = ? AND l.canceled_at IS NULL AND l.expires_at > now() AND l.enrollment_count < ?
         RETURNING l.id, l.expires_at)
INSERT INTO enrollments (link_id, session_hash, expires_at)
SELECT id, ?, expires_at FROM slot
RETURNING *`, linkToken, MaxLinkEnrollments, digest).Scan(&created)
	if result.Error != nil || result.RowsAffected == 0 {
		return nil, "", result.Error
	}
	s.changed()
	return &created, cookie, nil
}

// SaveDraft stores one validated registry field on an open form. Submitted forms never change.
// ponytail: the UPDATE commits with synchronous_commit off for its own transaction, so "Guardado." never waits
// for the WAL flush. A PostgreSQL crash can drop autosaves acknowledged in roughly the last 600 ms; nothing is
// corrupted or reordered, and Submit rewrites every contact column durably. Drop the async CTE to make drafts durable.
func (s *Store) SaveDraft(ctx context.Context, linkToken, cookieToken, key, value string) (SaveResult, error) {
	field, ok := students.FindField(key)
	if !ok {
		return Unavailable, students.ErrUnknownField
	}
	digest, ok := sessionDigest(linkToken, cookieToken)
	if !ok {
		return Unavailable, nil
	}
	result := s.db.WithContext(ctx).Exec(`
WITH async AS (SELECT set_config('synchronous_commit', 'off', true))
UPDATE enrollments e
   SET ? = ?
  FROM links l, async
 WHERE `+openEnrollment+`
   AND ? <> ?`,
		clause.Column{Name: field.Key}, value, linkToken, digest, clause.Column{Table: "e", Name: field.Key}, value)
	switch {
	case result.Error != nil:
		return Unavailable, result.Error
	case result.RowsAffected == 1:
		s.changed()
		return Saved, nil
	}
	return s.formState(ctx, linkToken, digest, Saved)
}

// Submit commits final answers and the PIN to the enrollment, locking the form, and spends one of its MaxPinChecks
// in the same UPDATE. A PIN held by an enrolled Student is refused here; one held by another submitted enrollment
// is refused by idx_enrollment_pin. A refused PIN still costs a check (PinTaken); a spent budget is PinLimited.
// No Student exists until Enroll.
// ponytail: a Submit racing a concurrent Enroll can still see the PIN free (READ COMMITTED); funding then fails on idx_student_pin. Upgrade: one PIN-reservation table or an advisory lock.
func (s *Store) Submit(ctx context.Context, linkToken, cookieToken string, details students.Details, pin string) (SaveResult, error) {
	digest, ok := sessionDigest(linkToken, cookieToken)
	if !ok {
		return Unavailable, nil
	}
	result := s.db.WithContext(ctx).Exec(`
UPDATE enrollments e
   SET name = ?, email = ?, phone = ?, municipality = ?, date_of_birth = ?, gender = ?, pin = ?,
       pin_checks = e.pin_checks + 1,
       submitted_at = now(),
       expires_at = now() + interval '168 hours'
  FROM links l
 WHERE `+openEnrollment+`
   AND e.pin_checks < ?
   AND NOT EXISTS (SELECT 1 FROM students WHERE pin = ? AND pin <> '')`,
		details.Name, details.Email, details.Phone, details.Municipality, details.DateOfBirth, details.Gender, pin, linkToken, digest, MaxPinChecks, pin)
	switch {
	case result.Error == nil && result.RowsAffected == 1:
		s.changed()
		return Saved, nil
	case result.Error != nil && !students.IsUniqueViolation(result.Error, pinIndex):
		return Unavailable, result.Error
	}
	// Nothing was written: the PIN is held, the budget is spent, or the form is not open.
	spent, err := s.spendPinCheck(ctx, linkToken, digest)
	switch {
	case err != nil:
		return Unavailable, err
	case spent:
		return PinTaken, nil
	}
	return s.formState(ctx, linkToken, digest, PinLimited)
}

// SpendPinCheck counts one PIN availability answer against the usable, unsubmitted enrollment
// behind (linkToken, cookieToken) and reports whether the enrollment still had budget.
func (s *Store) SpendPinCheck(ctx context.Context, linkToken, cookieToken string) (bool, error) {
	digest, ok := sessionDigest(linkToken, cookieToken)
	if !ok {
		return false, nil
	}
	return s.spendPinCheck(ctx, linkToken, digest)
}

func (s *Store) spendPinCheck(ctx context.Context, linkToken string, digest []byte) (bool, error) {
	result := s.db.WithContext(ctx).Exec(`
UPDATE enrollments e
   SET pin_checks = e.pin_checks + 1
  FROM links l
 WHERE `+openEnrollment+`
   AND e.pin_checks < ?`, linkToken, digest, MaxPinChecks)
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

// AcceptTerms records the agreement against the start date the customer was shown, once, after contact
// submission, and reports whether this call accepted. It refuses when no usable submitted enrollment still
// awaits its terms (so a replay never rewrites the first acceptance) or when shownStartDate is not the
// current program start date. Once submitted, the enrollment relies on its own expiry and cancellation
// rather than the parent link's.
func (s *Store) AcceptTerms(ctx context.Context, linkToken, cookieToken, shownStartDate string) (bool, error) {
	digest, ok := sessionDigest(linkToken, cookieToken)
	date, err := settings.ParseDate(shownStartDate)
	if !ok || err != nil {
		return false, nil
	}
	startDate := date.Format(time.DateOnly)
	// The settings row lock serializes acceptance with staff date changes. Correlating the subquery on e keeps
	// that FOR SHARE contract while locking settings only for a candidate row; uncorrelated, it plans as an
	// InitPlan that locks the row, and so writes WAL, on every POST.
	result := s.db.WithContext(ctx).Exec(`
UPDATE enrollments e
   SET terms_accepted_at = now(), program_start_date = ?
  FROM links l
 WHERE `+usableSession+` AND e.submitted_at IS NOT NULL AND e.terms_accepted_at IS NULL
   AND ? = (SELECT s.program_start_date FROM settings s WHERE s.id = 1 AND e.terms_accepted_at IS NULL FOR SHARE)`,
		startDate, linkToken, digest, startDate)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	s.changed()
	return true, nil
}

// formState classifies the form behind (linkToken, digest) in one query, by LookupSession's rule:
// a missing or unusable session is Unavailable.
func (s *Store) formState(ctx context.Context, linkToken string, digest []byte, open SaveResult) (SaveResult, error) {
	var submitted bool
	err := s.db.WithContext(ctx).Raw(`SELECT e.submitted_at IS NOT NULL FROM enrollments e, links l WHERE `+usableSession,
		linkToken, digest).Row().Scan(&submitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Unavailable, nil
	case err != nil:
		return Unavailable, err
	case submitted:
		return AlreadySubmitted, nil
	default:
		return open, nil
	}
}

// FormState reports, without writing, whether the form behind (linkToken, cookieToken) is Open,
// AlreadySubmitted or Unavailable, by the rule a write that touched no row uses.
func (s *Store) FormState(ctx context.Context, linkToken, cookieToken string) (SaveResult, error) {
	digest, ok := sessionDigest(linkToken, cookieToken)
	if !ok {
		return Unavailable, nil
	}
	return s.formState(ctx, linkToken, digest, Open)
}

// CancelLink revokes an active link so new visitors can no longer start enrollments on it.
// Unsubmitted enrollments under it derive Canceled; already-submitted enrollments keep their 168h window.
func (s *Store) CancelLink(ctx context.Context, id uint64, staff string) (*LinkEntry, error) {
	if staff == "" {
		return nil, errNoStaff
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
	entry := link.entry(time.Now())
	return &entry, nil
}

const maxLinkNameRunes = 100

// RenameLink updates a reusable link's display name on behalf of staff. An unknown ID returns (nil, nil).
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
	entry := link.entry(time.Now())
	return &entry, nil
}

// CancelEnrollmentTx cancels an InProgress or AwaitingPayment enrollment (clearing its PIN), or clears an
// Expired submission's PIN without turning it into Canceled. Any other enrollment is returned unchanged.
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
	out := e.entry(link, now)
	if !out.Cancelable() {
		return &out, false, nil
	}
	updates := map[string]any{"pin": ""}
	if out.Status != Expired {
		updates = canceledBy(staff)
		updates["pin"] = ""
	}
	upd := tx.Model(&e).Where("id = ? AND canceled_at IS NULL AND paid_at IS NULL", id).
		Clauses(clause.Returning{}).Updates(updates)
	if upd.Error != nil {
		return nil, false, upd.Error
	}
	out = e.entry(link, now)
	return &out, upd.RowsAffected > 0, nil
}

func canceledBy(staff string) map[string]any {
	return map[string]any{"canceled_at": gorm.Expr("now()"), "canceled_by": staff}
}

// openEnrollment is the WHERE guard of every customer write to an unsubmitted form: enrollment e and
// its link l are both Usable and e is not submitted. Its arguments are the link token, then the session digest.
const openEnrollment = `e.link_id = l.id
   AND l.token = ? AND l.canceled_at IS NULL AND l.expires_at > now()
   AND e.session_hash = ? AND e.canceled_at IS NULL AND e.expires_at > now()
   AND e.submitted_at IS NULL`

// usableSession matches enrollment e of link l for one session cookie while the customer may still use it:
// not canceled, unexpired unless paid, and an unsubmitted form also needs its link usable.
// Its arguments are the link token, then the session digest.
const usableSession = `e.link_id = l.id AND l.token = ? AND e.session_hash = ?
   AND e.canceled_at IS NULL AND (e.expires_at > now() OR e.paid_at IS NOT NULL)
   AND (e.submitted_at IS NOT NULL OR (l.canceled_at IS NULL AND l.expires_at > now()))`

// Usable scopes a query to rows that are neither canceled nor past their access deadline.
func Usable(db *gorm.DB) *gorm.DB {
	return db.Where("canceled_at IS NULL AND expires_at > now()")
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

// sessionDigest checks both customer tokens before any query and returns the cookie token's digest.
func sessionDigest(linkToken, cookieToken string) ([]byte, bool) {
	if !validToken(linkToken) {
		return nil, false
	}
	return tokenDigest(cookieToken)
}
