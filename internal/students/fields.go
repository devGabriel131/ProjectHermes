package students

import (
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nyaruka/phonenumbers/v2"

	"projecthermes/internal/puertorico"
)

type Field struct {
	Key, Label, InputType string
	Value                 func(Student) string
	Parse                 func(raw string, required bool) (string, error)
	Options               []string
}

// fields are the contact fields: a customer submits them, a Link drafts them, a Student holds them.
var fields = []Field{
	{
		Key: "name", Label: "Name", InputType: "text",
		Value: func(s Student) string { return s.Name },
		Parse: ParseName,
	},
	{
		Key: "email", Label: "Email", InputType: "email",
		Value: func(s Student) string { return s.Email },
		Parse: ParseEmail,
	},
	{
		Key: "phone", Label: "Phone", InputType: "tel",
		Value: func(s Student) string { return s.Phone },
		Parse: ParsePhone,
	},
	{
		Key: "municipality", Label: "Municipality",
		Value: func(s Student) string { return s.Municipality },
		Parse: ParseMunicipality, Options: MunicipalityOptions(),
	},
	{
		Key: "date_of_birth", Label: "Date of birth", InputType: "date",
		Value: func(s Student) string { return s.DateOfBirth },
		Parse: ParseDateOfBirth,
	},
	{
		Key: "gender", Label: "Gender",
		Value: func(s Student) string { return s.Gender },
		Parse: ParseGender, Options: genders,
	},
}

// pinField is staff-only and never drafted: the customer enters it once, at final submit, so abandoned drafts never reserve a PIN. The Link holds it from submit until payment; the Student holds it afterwards.
var pinField = Field{
	Key: "pin", Label: "PIN", InputType: "text",
	Value: func(s Student) string { return s.Pin },
	Parse: ParsePin,
}

// groupField is the program stage, staff-only like the PIN: never drafted or customer-edited.
var groupField = Field{
	Key: "group", Label: "Group",
	Value: func(s Student) string { return s.Group },
	Parse: ParseGroup, Options: groups,
}

var staffFields = append(slices.Clone(fields), pinField, groupField)

// Fields returns the ordered contact registry. Callers must not modify the slice.
func Fields() []Field {
	return fields
}

// StaffFields is Fields plus the PIN and group, the registry staff see and edit.
// Callers must not modify the slice.
func StaffFields() []Field {
	return staffFields
}

func FindField(key string) (Field, bool) {
	return find(fields, key)
}

// FindStaffField is FindField that also finds the staff-only fields.
func FindStaffField(key string) (Field, bool) {
	return find(staffFields, key)
}

func find(registry []Field, key string) (Field, bool) {
	for _, field := range registry {
		if field.Key == key {
			return field, true
		}
	}
	return Field{}, false
}

// ParseName validates full names while allowing empty or one-word drafts.
func ParseName(raw string, required bool) (string, error) {
	const message = "Enter a full name using letters and spaces."
	if !utf8.ValidString(raw) {
		return "", &ValidationError{Message: message}
	}
	name := strings.TrimSpace(raw)
	if name == "" {
		if required {
			return "", &ValidationError{Message: message}
		}
		return "", nil
	}
	words, inWord := 0, false
	lastWasDot := false
	normalize := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r):
			if !inWord || lastWasDot {
				words++
			}
			inWord = true
			lastWasDot = false
		case unicode.IsMark(r) && inWord && !lastWasDot:
		case r == '.' && inWord && !lastWasDot:
			lastWasDot = true
		case unicode.Is(unicode.Zs, r):
			normalize = normalize || r != ' ' || !inWord
			inWord = false
			lastWasDot = false
		default:
			return "", &ValidationError{Message: message}
		}
	}
	if required && words < 2 {
		return "", &ValidationError{Message: message}
	}
	if !normalize {
		return name, nil
	}
	var normalized strings.Builder
	normalized.Grow(len(name))
	inWord = false
	for _, r := range name {
		if unicode.Is(unicode.Zs, r) {
			if inWord {
				normalized.WriteByte(' ')
			}
			inWord = false
			continue
		}
		normalized.WriteRune(r)
		inWord = true
	}
	return normalized.String(), nil
}

// ParseEmail accepts one bare ASCII mailbox and lowercases only its domain.
func ParseEmail(raw string, required bool) (string, error) {
	const message = "Enter a valid email address."
	email := strings.TrimSpace(raw)
	if email == "" && !required {
		return "", nil
	}
	if email == "" || len(email) > 254 || strings.ContainsRune(email, '"') {
		return "", &ValidationError{Message: message}
	}
	for i := range len(email) {
		if email[i] <= ' ' || email[i] >= 127 {
			return "", &ValidationError{Message: message}
		}
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Name != "" || address.Address != email {
		return "", &ValidationError{Message: message}
	}
	at := strings.LastIndexByte(email, '@')
	if at < 1 || at > 64 {
		return "", &ValidationError{Message: message}
	}
	domain := email[at+1:]
	if !strings.ContainsRune(domain, '.') {
		return "", &ValidationError{Message: message}
	}
	for label := range strings.SplitSeq(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", &ValidationError{Message: message}
		}
		for i := range len(label) {
			c := label[i]
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
				return "", &ValidationError{Message: message}
			}
		}
	}
	normalizedDomain := strings.ToLower(domain)
	if normalizedDomain == domain {
		return email, nil
	}
	return email[:at+1] + normalizedDomain, nil
}

// ParsePhone accepts metadata-valid PR or US numbers and returns E.164.
func ParsePhone(raw string, required bool) (string, error) {
	const message = "Enter a valid Puerto Rico or United States phone number with area code."
	phone := strings.TrimSpace(raw)
	if phone == "" && !required {
		return "", nil
	}
	if phone == "" {
		return "", &ValidationError{Message: message}
	}
	for i := range len(phone) {
		c := phone[i]
		if !('0' <= c && c <= '9' || c == ' ' || c == '(' || c == ')' || c == '-' || c == '.' || c == '+' && i == 0) {
			return "", &ValidationError{Message: message}
		}
	}
	number, err := phonenumbers.Parse(phone, "PR")
	if err != nil {
		return "", &ValidationError{Message: message}
	}
	region := phonenumbers.GetRegionCodeForNumber(number)
	if (region != "PR" && region != "US") || !phonenumbers.IsValidNumberForRegion(number, region) {
		return "", &ValidationError{Message: message}
	}
	return phonenumbers.Format(number, phonenumbers.E164), nil
}

// ParseDateOfBirth validates a calendar date formatted as YYYY-MM-DD.
// The date must be strictly before today in Puerto Rico with age 0-100 inclusive.
func ParseDateOfBirth(raw string, required bool) (string, error) {
	const message = "Enter a date of birth before today and no more than 100 years ago."
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" && !required {
		return "", nil
	}
	t, err := time.ParseInLocation(time.DateOnly, trimmed, puertorico.Zone)
	if err != nil {
		return "", &ValidationError{Message: message}
	}
	nowYear, nowMonth, nowDay := time.Now().In(puertorico.Zone).Date()
	// Midnight today, so a birth date equal to today is not Before it.
	today := time.Date(nowYear, nowMonth, nowDay, 0, 0, 0, 0, puertorico.Zone)
	if !t.Before(today) {
		return "", &ValidationError{Message: message}
	}
	birthYear, birthMonth, birthDay := t.Date()
	age := nowYear - birthYear
	if nowMonth < birthMonth || (nowMonth == birthMonth && nowDay < birthDay) {
		age--
	}
	if age > 100 {
		return "", &ValidationError{Message: message}
	}
	return t.Format(time.DateOnly), nil
}

var genders = []string{"Male", "Female"}

func ParseGender(raw string, required bool) (string, error) {
	return parseOption(raw, required, genders, "Select Male or Female.")
}

// groups are the program stages in order. Enroll starts students in the first; Add uses the one staff choose.
// Student.Group's column default and check repeat them.
var groups = []string{"Básico", "Regular"}

// GroupOptions returns the ordered groups. Callers must not modify the slice.
func GroupOptions() []string {
	return groups
}

func ParseGroup(raw string, required bool) (string, error) {
	return parseOption(raw, required, groups, "Select Básico or Regular.")
}

// parseOption accepts exactly one of options, ignoring surrounding space.
func parseOption(raw string, required bool, options []string, message string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" && !required {
		return "", nil
	}
	if slices.Contains(options, value) {
		return value, nil
	}
	return "", &ValidationError{Message: message}
}

// ParsePin accepts 4 to 6 ASCII digits. It stays text so leading zeros survive.
func ParsePin(raw string, required bool) (string, error) {
	const message = "Enter a PIN of 4 to 6 digits."
	pin := strings.TrimSpace(raw)
	if pin == "" && !required {
		return "", nil
	}
	if len(pin) < 4 || len(pin) > 6 {
		return "", &ValidationError{Message: message}
	}
	for i := range len(pin) {
		if pin[i] < '0' || pin[i] > '9' {
			return "", &ValidationError{Message: message}
		}
	}
	return pin, nil
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
