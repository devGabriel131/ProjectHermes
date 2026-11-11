package management

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"projecthermes/internal/students"
)

func TestReadRosterAndFixWorkbookRoundTrip(t *testing.T) {
	f := excelize.NewFile()
	defer f.Close()

	header := []string{"Last name", "First name", "Student ID", "Email"}
	rows := [][]string{
		header,
		{"Gonzalez", "Alberto", "'0388'", "a@example.com"},
		{"Zapata", "Amarilys", "'2556'"}, // no D cell
		{},                               // empty
		{"Silvestrini Alicea", "Mirayda E.", "'7247'", "m@example.com"},
	}

	for i, r := range rows {
		cell, err := excelize.CoordinatesToCellName(1, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if len(r) > 0 {
			if err := f.SetSheetRow("Sheet1", cell, &r); err != nil {
				t.Fatal(err)
			}
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}

	gotHeader, gotRows, err := readRoster("roster.xlsx", buf)
	if err != nil {
		t.Fatalf("readRoster(xlsx): %v", err)
	}

	if !reflect.DeepEqual(gotHeader, header) {
		t.Fatalf("gotHeader = %v, want %v", gotHeader, header)
	}

	wantLines := []int{2, 3, 5}
	if len(gotRows) != len(wantLines) {
		t.Fatalf("len(gotRows) = %d, want %d", len(gotRows), len(wantLines))
	}
	for i, line := range wantLines {
		if gotRows[i].Line != line {
			t.Fatalf("gotRows[%d].Line = %d, want %d", i, gotRows[i].Line, line)
		}
	}

	wantEntries := []students.NewStudent{
		{Name: "Alberto Gonzalez", Email: "a@example.com", Pin: "0388"},
		{Name: "Amarilys Zapata", Email: "", Pin: "2556"},
		{Name: "Mirayda E. Silvestrini Alicea", Email: "m@example.com", Pin: "7247"},
	}
	for i, want := range wantEntries {
		if entry := gotRows[i].entry(); entry != want {
			t.Fatalf("gotRows[%d].entry() = %+v, want %+v", i, entry, want)
		}
	}

	// CSV with BOM
	csvContent := "\ufeffLast name,First name,Student ID,Email\r\nGonzalez,Alberto,'0388',a@example.com\r\n"
	csvHeader, csvRows, err := readRoster("roster.csv", strings.NewReader(csvContent))
	if err != nil {
		t.Fatalf("readRoster(csv): %v", err)
	}
	if !reflect.DeepEqual(csvHeader, header) {
		t.Fatalf("csvHeader = %v, want %v", csvHeader, header)
	}
	if len(csvRows) != 1 || csvRows[0].Line != 2 {
		t.Fatalf("csvRows = %+v, want 1 row at line 2", csvRows)
	}
	if csvEntry := csvRows[0].entry(); csvEntry != wantEntries[0] {
		t.Fatalf("csvEntry = %+v, want %+v", csvEntry, wantEntries[0])
	}

	// Round trip fix workbook
	rejected := []rejectedRow{
		{
			Line:     gotRows[1].Line,
			Name:     wantEntries[1].Name,
			Cells:    gotRows[1].Cells,
			Problems: []string{"Enter a valid email address."},
		},
		{
			Line:     gotRows[2].Line,
			Name:     wantEntries[2].Name,
			Cells:    gotRows[2].Cells,
			Problems: []string{"Enter a full name using letters and spaces."},
		},
	}
	fixBytes, err := fixWorkbook(gotHeader, rejected)
	if err != nil {
		t.Fatalf("fixWorkbook: %v", err)
	}

	fixHeader, fixRows, err := readRoster("fix.xlsx", bytes.NewReader(fixBytes))
	if err != nil {
		t.Fatalf("readRoster(fix.xlsx): %v", err)
	}
	if !reflect.DeepEqual(fixHeader, header) {
		t.Fatalf("fixHeader = %v, want %v (Errors column ignored)", fixHeader, header)
	}
	if len(fixRows) != 2 {
		t.Fatalf("len(fixRows) = %d, want 2", len(fixRows))
	}
	if fixRows[0].entry() != wantEntries[1] {
		t.Fatalf("fixRows[0].entry() = %+v, want %+v", fixRows[0].entry(), wantEntries[1])
	}
	if fixRows[1].entry() != wantEntries[2] {
		t.Fatalf("fixRows[1].entry() = %+v, want %+v", fixRows[1].entry(), wantEntries[2])
	}

	// Error cases
	// 1. Non-xlsx/csv file
	_, _, err = readRoster("roster.pdf", strings.NewReader("dummy"))
	var valErr *students.ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *students.ValidationError for .pdf, got %v", err)
	}

	// 2. Header-only CSV
	_, _, err = readRoster("header.csv", strings.NewReader("Last name,First name,Student ID,Email\r\n"))
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *students.ValidationError for header-only CSV, got %v", err)
	}

	// 3. More than maxRosterRows
	var sb strings.Builder
	sb.WriteString("Last name,First name,Student ID,Email\r\n")
	for range maxRosterRows + 1 {
		sb.WriteString("Last,First,'1234',email@example.com\r\n")
	}
	_, _, err = readRoster("toolarge.csv", strings.NewReader(sb.String()))
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *students.ValidationError for >maxRosterRows CSV, got %v", err)
	}
}
