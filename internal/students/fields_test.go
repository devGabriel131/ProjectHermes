package students_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"projecthermes/internal/students"
)

func TestFieldParsersRespectNormalizationAndDraftBoundaries(t *testing.T) {
	t.Parallel()
	type parserCase struct {
		name     string
		raw      string
		required bool
		want     string
		invalid  bool
	}
	local64 := strings.Repeat("a", 64)
	puertoRico := time.FixedZone("America/Puerto_Rico", -4*60*60)
	now := time.Now().In(puertoRico)
	relative := func(years, days int) string { return now.AddDate(years, 0, days).Format("2006-01-02") }
	dobCases := []parserCase{
		{"valid date", "2005-06-15", true, "2005-06-15", false},
		{"trimmed valid draft", " \t2005-06-15\u2003\n", false, "2005-06-15", false},
		{"empty draft", "", false, "", false},
		{"blank draft", " \t\u2003\n", false, "", false},
		{"empty final", "", true, "", true},
		{"blank final", " \t\u2003\n", true, "", true},
		{"not a date", "not-a-date", true, "", true},
		{"slashes", "2005/06/15", true, "", true},
		{"US order", "06-15-2005", false, "", true},
		{"unpadded", "2005-6-15", true, "", true},
		{"impossible day", "2005-02-30", true, "", true},
		{"time suffix", "2005-06-15T00:00:00Z", true, "", true},
		{"NUL", "2005-06-15\x00", true, "", true},
		{"future year", "2099-01-01", true, "", true},
		{"future draft", "2099-01-01", false, "", true},
		{"over 100", "1900-01-01", true, "", true},
		{"yesterday", relative(0, -1), true, relative(0, -1), false},
		{"today", relative(0, 0), true, "", true},
		{"tomorrow", relative(0, 1), true, "", true},
	}
	// AddDate rolls Feb 29 back a century to Mar 1, which shifts the age boundary.
	if now.Month() != time.February || now.Day() != 29 {
		dobCases = append(dobCases,
			parserCase{"turned 101 today", relative(-101, 0), true, "", true},
			parserCase{"turns 101 tomorrow", relative(-101, 1), true, relative(-101, 1), false},
		)
	}
	domain189 := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 61)
	for _, parser := range []struct {
		name  string
		parse func(string, bool) (string, error)
		cases []parserCase
	}{
		{"name", students.ParseName, []parserCase{
			{"accented full name", "María José Pérez Rivera", true, "María José Pérez Rivera", false},
			{"decomposed accents preserved", " Mari\u0301a Jose\u0301 Pe\u0301rez Rivera ", true, "Mari\u0301a Jose\u0301 Pe\u0301rez Rivera", false},
			{"multiple attached combining marks", "A\u0301\u0327na Rivera", true, "A\u0301\u0327na Rivera", false},
			{"Unicode separator normalization", "\t\n María\u00a0\u2003José   Pérez\u202fRivera \r\n", true, "María José Pérez Rivera", false},
			{"Unicode letters preserved", "李 小龍", true, "李 小龍", false},
			{"one word draft", " María ", false, "María", false},
			{"one word final", "María", true, "", true},
			{"empty draft", "", false, "", false},
			{"blank draft", " \t\u2003\n", false, "", false},
			{"empty final", "", true, "", true},
			{"blank final", " \t\u2003\n", true, "", true},
			{"no independent name size ceiling", strings.Repeat("Ana", 1500) + " Rivera", true, strings.Repeat("Ana", 1500) + " Rivera", false},
			{"ASCII digits", "Ana Rivera2", true, "", true},
			{"Unicode digits", "Ana Rivera٢", false, "", true},
			{"hyphen", "Ana María-Rivera", true, "", true},
			{"apostrophe", "Ana O'Connor", true, "", true},
			{"curly apostrophe", "Ana O’Connor", false, "", true},
			{"markup", "<b>Ana Rivera</b>", true, "", true},
			{"ampersand", "Ana & Rivera", true, "", true},
			{"emoji", "Ana Rivera😀", false, "", true},
			{"leading detached combining mark", "\u0301Ana Rivera", true, "", true},
			{"word starts with combining mark", "Ana \u0301Rivera", false, "", true},
			{"only combining marks", "\u0301\u0327", false, "", true},
			{"internal newline", "Ana\nRivera", true, "", true},
			{"internal carriage return", "Ana\rRivera", true, "", true},
			{"internal tab", "Ana\tRivera", false, "", true},
			{"internal vertical tab", "Ana\vRivera", true, "", true},
			{"internal form feed", "Ana\fRivera", true, "", true},
			{"Unicode line separator", "Ana\u2028Rivera", true, "", true},
			{"Unicode paragraph separator", "Ana\u2029Rivera", true, "", true},
			{"zero width separator", "Ana\u200bRivera", true, "", true},
			{"bidi control", "Ana \u202eRivera", false, "", true},
			{"NUL", "Ana\x00 Rivera", true, "", true},
			{"NUL draft", "\x00", false, "", true},
			{"invalid UTF8", "Ana\xff Rivera", true, "", true},
			{"invalid UTF8 draft before trim", " \xff ", false, "", true},
		}},
		{"email", students.ParseEmail, []parserCase{
			{"plus dots and local case", " \tAda.Lovelace+Class@EXAMPLE.COM\n", true, "Ada.Lovelace+Class@example.com", false},
			{"bare draft mailbox", "Ada@Example.COM", false, "Ada@example.com", false},
			{"ASCII punycode domain", "Ada@XN--BCHER-KVA.EXAMPLE", true, "Ada@xn--bcher-kva.example", false},
			{"hyphen and digits in labels", "Ada@mail-2.example", true, "Ada@mail-2.example", false},
			{"unquoted apostrophe is valid local text", "O'Connor@example.com", true, "O'Connor@example.com", false},
			{"64 byte local part", local64 + "@example.com", true, local64 + "@example.com", false},
			{"254 byte mailbox and 63 byte labels", local64 + "@" + domain189, true, local64 + "@" + domain189, false},
			{"65 byte local part", local64 + "a@example.com", true, "", true},
			{"255 byte mailbox", local64 + "@" + domain189 + "c", true, "", true},
			{"64 byte domain label", "Ada@" + strings.Repeat("a", 64) + ".com", false, "", true},
			{"empty draft", "", false, "", false},
			{"blank draft", " \t\u2003\n", false, "", false},
			{"empty final", "", true, "", true},
			{"blank final", " \t\u2003\n", true, "", true},
			{"display name", "Ada <Ada@example.com>", true, "", true},
			{"bare angle brackets", "<Ada@example.com>", true, "", true},
			{"address list", "Ada@example.com, Other@example.com", false, "", true},
			{"group", "Class: Ada@example.com;", true, "", true},
			{"trailing comment", "Ada@example.com (Student)", true, "", true},
			{"local comment", "Ada(Student)@example.com", true, "", true},
			{"quoted local part", "\"Ada\"@example.com", true, "", true},
			{"quoted local part requiring quotes", "\"Ada Rivera\"@example.com", true, "", true},
			{"address literal", "Ada@[127.0.0.1]", true, "", true},
			{"IPv6 address literal", "Ada@[IPv6:2001:db8::1]", true, "", true},
			{"dotless domain", "Ada@localhost", true, "", true},
			{"empty domain label", "Ada@example..com", false, "", true},
			{"leading empty label", "Ada@.example.com", true, "", true},
			{"trailing empty label", "Ada@example.com.", true, "", true},
			{"leading label hyphen", "Ada@-example.com", true, "", true},
			{"trailing label hyphen", "Ada@example-.com", true, "", true},
			{"domain underscore", "Ada@my_example.com", true, "", true},
			{"domain punctuation", "Ada@example!.com", false, "", true},
			{"Unicode local part", "María@example.com", true, "", true},
			{"Unicode domain", "Ada@ejémplo.com", true, "", true},
			{"internal space", "Ada @example.com", true, "", true},
			{"internal tab", "Ada@exam\tple.com", false, "", true},
			{"header injection", "Ada@example.com\r\nBcc: private@example.com", true, "", true},
			{"NUL", "Ada\x00@example.com", true, "", true},
			{"DEL", "Ada\x7f@example.com", true, "", true},
			{"invalid UTF8", "Ada\xff@example.com", false, "", true},
			{"multiple at signs", "Ada@@example.com", true, "", true},
			{"empty local part", "@example.com", true, "", true},
			{"leading local dot", ".Ada@example.com", true, "", true},
			{"trailing local dot", "Ada.@example.com", true, "", true},
			{"repeated local dot", "Ada..Rivera@example.com", true, "", true},
		}},
		{"phone", students.ParsePhone, []parserCase{
			{"PR 787 metadata example", "7872345678", true, "+17872345678", false},
			{"PR 939 valid metadata prefix", "(939) 234-5678", true, "+19392345678", false},
			{"PR leading one", "1 787 234 5678", true, "+17872345678", false},
			{"PR international", " +1 (787) 234-5678 ", false, "+17872345678", false},
			{"US metadata example", "2015550123", true, "+12015550123", false},
			{"US formatted", "(202) 555-0123", true, "+12025550123", false},
			{"US dot separators", "1.650.253.0000", true, "+16502530000", false},
			{"surrounding whitespace", "\t\u2003+1 201 555 0123\n", true, "+12015550123", false},
			{"empty draft", "", false, "", false},
			{"blank draft", " \t\u2003\n", false, "", false},
			{"empty final", "", true, "", true},
			{"blank final", " \t\u2003\n", true, "", true},
			{"short emergency", "911", true, "", true},
			{"local without area code", "2345678", false, "", true},
			{"incomplete", "787234567", true, "", true},
			{"metadata invalid PR exchange", "7871234567", true, "", true},
			{"metadata invalid US area", "2002345678", true, "", true},
			{"Canada metadata example", "+15062345678", true, "", true},
			{"Canada Toronto", "+1 416 555 0123", false, "", true},
			{"other NANP Bahamas", "+1 242 365 1234", true, "", true},
			{"foreign country", "+44 20 7946 0018", true, "", true},
			{"vanity letters", "1-800-FLOWERS", true, "", true},
			{"extension x", "7872345678 x123", true, "", true},
			{"extension text", "7872345678 ext. 123", false, "", true},
			{"extension hash", "7872345678#123", true, "", true},
			{"misplaced plus", "1+7872345678", true, "", true},
			{"duplicate plus", "++17872345678", true, "", true},
			{"Unicode digits", "７８７２３４５６７８", true, "", true},
			{"internal Unicode space", "787\u00a0234 5678", true, "", true},
			{"internal tab", "787\t2345678", false, "", true},
			{"internal newline", "787\n2345678", true, "", true},
			{"NUL", "7872345678\x00", true, "", true},
			{"invalid UTF8", "787\xff2345678", true, "", true},
		}},
		{"municipality", students.ParseMunicipality, []parserCase{
			{"accented choice", "Añasco", true, "Añasco", false},
			{"trimmed accented choice", "\t Bayamón \u2003", false, "Bayamón", false},
			{"outside", "Fuera de Puerto Rico", true, "Fuera de Puerto Rico", false},
			{"empty draft", "", false, "", false},
			{"blank draft", " \t\u2003\n", false, "", false},
			{"empty final", "", true, "", true},
			{"blank final", " \t\u2003\n", true, "", true},
			{"forged choice", "PRIVATE_FORGED_MUNICIPALITY", true, "", true},
			{"forged draft choice", "Not a municipality", false, "", true},
			{"alternate case", "san juan", true, "", true},
			{"missing accent", "Anasco", true, "", true},
			{"decomposed spelling is not exact", "An\u0303asco", true, "", true},
			{"alternate outside spelling", "Outside Puerto Rico", true, "", true},
			{"markup", "<option>San Juan</option>", true, "", true},
			{"internal whitespace", "San  Juan", true, "", true},
			{"NUL", "San Juan\x00", true, "", true},
			{"invalid UTF8", "San Juan\xff", true, "", true},
		}},
		{"date of birth", students.ParseDateOfBirth, dobCases},
		{"gender", students.ParseGender, []parserCase{
			{"male", "Male", true, "Male", false},
			{"trimmed female", " \tFemale\u2003", true, "Female", false},
			{"empty draft", "", false, "", false},
			{"empty final", "", true, "", true},
			{"alternate case", "male", true, "", true},
			{"other value", "Other", true, "", true},
			{"forged draft", "PRIVATE_FORGED_GENDER", false, "", true},
		}},
		{"pin", students.ParsePin, []parserCase{
			{"shortest", "1234", true, "1234", false},
			{"longest", "123456", true, "123456", false},
			{"leading zeros kept", "000042", true, "000042", false},
			{"trimmed", " \t4321\u2003", true, "4321", false},
			{"empty draft", "", false, "", false},
			{"empty final", "", true, "", true},
			{"too short", "123", true, "", true},
			{"too long", "1234567", true, "", true},
			{"letters", "12a45", true, "", true},
			{"internal space", "12 34", true, "", true},
			{"sign", "+1234", true, "", true},
			{"fullwidth digits", "１２３４", true, "", true},
			{"arabic-indic digits", "١٢٣٤", true, "", true},
			{"NUL", "1234\x00", true, "", true},
		}},
	} {
		t.Run(parser.name, func(t *testing.T) {
			for _, tc := range parser.cases {
				t.Run(tc.name, func(t *testing.T) {
					got, err := parser.parse(tc.raw, tc.required)
					if got != tc.want {
						t.Fatalf("parsed %q, want %q", got, tc.want)
					}
					if !tc.invalid {
						if err != nil {
							t.Fatal(err)
						}
						return
					}
					var invalid *students.ValidationError
					if !errors.As(err, &invalid) || invalid.Message == "" || err.Error() != invalid.Message {
						t.Fatalf("error = %v, want nonempty ValidationError", err)
					}
				})
			}
		})
	}
}

func TestParserErrorsNeverEchoSubmittedValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key, private string
	}{
		{"name", "PRIVATE_NAME_123"},
		{"email", "PRIVATE_EMAIL_AT_PRIVATE_DOMAIN"},
		{"phone", "PRIVATE_PHONE_VALUE"},
		{"municipality", "PRIVATE_MUNICIPALITY_VALUE"},
		{"date_of_birth", "PRIVATE_DOB_VALUE"},
		{"gender", "PRIVATE_GENDER_VALUE"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			field, ok := students.FindField(tc.key)
			if !ok {
				t.Fatal("missing field")
			}
			_, err := field.Parse(tc.private, true)
			var invalid *students.ValidationError
			if !errors.As(err, &invalid) || strings.Contains(err.Error(), tc.private) {
				t.Fatalf("validation error leaked submitted value or lost type: %v", err)
			}
		})
	}
}
