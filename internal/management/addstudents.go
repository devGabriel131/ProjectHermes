package management

import (
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/xuri/excelize/v2"

	"apollo/internal/logging/httplog"
	"apollo/internal/students"
)

// Roster columns by position (Socrative roster export). Later columns are ignored,
// so a fix file's Errors column uploads again cleanly.
const (
	rosterLastName = iota
	rosterFirstName
	rosterPin
	rosterEmail
	rosterColumns
)

// maxRosterRows bounds one upload so validation and insert finish well inside the server's 10 s WriteTimeout.
const maxRosterRows = 1000

var errUnreadableRoster = &students.ValidationError{Message: "Could not read this file. Upload an .xlsx or .csv roster."}

type rosterRow struct {
	Line  int      // spreadsheet row number, 1-based
	Cells []string // exactly rosterColumns cells, "" where missing
}

func (row rosterRow) entry() students.NewStudent {
	return students.NewStudent{
		Name:  strings.TrimSpace(row.Cells[rosterFirstName] + " " + row.Cells[rosterLastName]),
		Email: row.Cells[rosterEmail],
		Pin:   strings.Trim(strings.TrimSpace(row.Cells[rosterPin]), "'"),
	}
}

// rejectedRow is a roster row Add refused, as the report and the fix workbook show it.
type rejectedRow struct {
	Line     int
	Name     string   // NewStudent.Name as built from the row
	Cells    []string // the row's roster columns as uploaded
	Problems []string
}

type rosterReport struct {
	Added    int
	Rejected []rejectedRow
	FixFile  []byte // the fix workbook; nil when nothing was rejected
}

// summary is the report's status line for an import into group.
func (r rosterReport) summary(group string) string {
	line := fmt.Sprintf("Added %d %s to %s.", r.Added, plural(r.Added, "student", "students"), group)
	if n := len(r.Rejected); n > 0 {
		line += fmt.Sprintf(" %d %s fixing.", n, plural(n, "row needs", "rows need"))
	}
	return line
}

// xlsxDataURL is the download URL of an .xlsx workbook. SafeURL skips templ's scheme check, which refuses data:;
// it is safe because the prefix is constant and base64's alphabet cannot leave the href or change its scheme.
func xlsxDataURL(workbook []byte) templ.SafeURL {
	return templ.SafeURL("data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64," + base64.StdEncoding.EncodeToString(workbook))
}

type addStudentsData struct {
	Staff       string
	Groups      []string            // students.GroupOptions(), for both Group selects
	Entry       students.NewStudent // values redisplayed after a refusal
	AddGroup    string              // the single form's group: redisplayed after a refusal, kept after an add
	Errors      students.Rejection  // per-field messages after a refusal
	AddError    string              // refusal not tied to a text field: an invalid group or a lost PIN race
	Added       *students.Student   // the student named by ?added=
	ImportGroup string              // the import form's group: redisplayed after a refusal, kept after an import
	ImportError string
	Imported    *rosterReport
}

func readRoster(filename string, file io.Reader) ([]string, []rosterRow, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	var rawRows [][]string

	switch ext {
	case ".xlsx":
		f, err := excelize.OpenReader(file, excelize.Options{UnzipSizeLimit: 32 << 20, UnzipXMLSizeLimit: 32 << 20})
		if err != nil {
			return nil, nil, errUnreadableRoster
		}
		defer f.Close()

		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, nil, errUnreadableRoster
		}
		rows, err := f.GetRows(sheets[0])
		if err != nil {
			return nil, nil, errUnreadableRoster
		}
		rawRows = rows
	case ".csv":
		r := csv.NewReader(file)
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil {
			return nil, nil, errUnreadableRoster
		}
		if len(rows) > 0 && len(rows[0]) > 0 {
			rows[0][0] = strings.TrimPrefix(rows[0][0], "\ufeff")
		}
		rawRows = rows
	default:
		return nil, nil, &students.ValidationError{Message: "Upload an .xlsx or .csv file."}
	}

	normalize := func(cells []string) []string {
		norm := make([]string, rosterColumns)
		for i := 0; i < len(cells) && i < rosterColumns; i++ {
			norm[i] = cells[i]
		}
		return norm
	}

	isBlank := func(cells []string) bool {
		for _, c := range cells {
			if strings.TrimSpace(c) != "" {
				return false
			}
		}
		return true
	}

	var header []string
	var studentRows []rosterRow

	for rawIdx, cells := range rawRows {
		if isBlank(cells) {
			continue
		}
		if header == nil {
			header = normalize(cells)
			continue
		}
		studentRows = append(studentRows, rosterRow{
			Line:  rawIdx + 1,
			Cells: normalize(cells),
		})
	}

	if header == nil || len(studentRows) == 0 {
		return nil, nil, &students.ValidationError{Message: "The file has no student rows below its header."}
	}
	if len(studentRows) > maxRosterRows {
		return nil, nil, &students.ValidationError{Message: "The file has more than 1000 student rows. Split it and upload each part."}
	}

	return header, studentRows, nil
}

func fixWorkbook(header []string, rows []rejectedRow) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	headerCells := append(slices.Clone(header), "Errors")
	cell, err := excelize.CoordinatesToCellName(1, 1)
	if err != nil {
		return nil, err
	}
	if err := f.SetSheetRow("Sheet1", cell, &headerCells); err != nil {
		return nil, err
	}

	for i, r := range rows {
		rowCells := append(slices.Clone(r.Cells), strings.Join(r.Problems, " "))
		cell, err := excelize.CoordinatesToCellName(1, i+2)
		if err != nil {
			return nil, err
		}
		if err := f.SetSheetRow("Sheet1", cell, &rowCells); err != nil {
			return nil, err
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *site) addStudentsPage(w http.ResponseWriter, r *http.Request) {
	data := addStudentsData{Staff: s.staff(r.Context()), Groups: students.GroupOptions()}
	if rawID := r.URL.Query().Get("added"); rawID != "" {
		if id, err := strconv.ParseUint(rawID, 10, 63); err == nil {
			data.Added, err = s.roster.Get(r.Context(), id)
			if err != nil {
				httplog.RecordError(r, "load", err)
				http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
				return
			}
			if data.Added != nil {
				data.AddGroup = data.Added.Group
			}
		}
	}
	s.view(w, r, http.StatusOK, addStudentsView(data))
}

func (s *site) addStudent(w http.ResponseWriter, r *http.Request) {
	if !readForm(w, r) {
		return
	}
	entry := students.NewStudent{Name: r.PostForm.Get("name"), Email: r.PostForm.Get("email"), Pin: r.PostForm.Get("pin")}
	data := addStudentsData{Staff: s.staff(r.Context()), Groups: students.GroupOptions(), Entry: entry, AddGroup: r.PostForm.Get("group")}
	group, err := students.ParseGroup(data.AddGroup, true)
	if err != nil {
		data.AddError = err.Error()
		s.view(w, r, http.StatusUnprocessableEntity, addStudentsView(data))
		return
	}
	added, rejected, err := s.roster.Add(r.Context(), group, []students.NewStudent{entry})

	var valErr *students.ValidationError
	switch {
	case errors.As(err, &valErr):
		data.AddError = valErr.Message
		s.view(w, r, http.StatusConflict, addStudentsView(data))
	case err != nil:
		httplog.RecordError(r, "save", err)
		http.Error(w, "Could not add the student. Please try again.", http.StatusInternalServerError)
	case len(rejected) > 0:
		data.Errors = rejected[0]
		s.view(w, r, http.StatusUnprocessableEntity, addStudentsView(data))
	default:
		http.Redirect(w, r, "/management/students/new?added="+strconv.FormatUint(added[0].ID, 10), http.StatusSeeOther)
	}
}

func (s *site) importStudents(w http.ResponseWriter, r *http.Request) {
	data := addStudentsData{Staff: s.staff(r.Context()), Groups: students.GroupOptions()}
	refuse := func(status int, message string) {
		data.ImportError = message
		s.view(w, r, status, addStudentsView(data))
	}

	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(http.StatusRequestEntityTooLarge, "The file is larger than 2 MB.")
		} else {
			refuse(http.StatusUnprocessableEntity, "Choose an .xlsx or .csv file to upload.")
		}
		return
	}

	data.ImportGroup = r.PostForm.Get("group")
	group, err := students.ParseGroup(data.ImportGroup, true)
	if err != nil {
		refuse(http.StatusUnprocessableEntity, err.Error())
		return
	}
	data.ImportGroup = group

	file, header, err := r.FormFile("roster")
	if err != nil {
		refuse(http.StatusUnprocessableEntity, "Choose an .xlsx or .csv file to upload.")
		return
	}
	defer file.Close()

	head, rows, err := readRoster(header.Filename, file)
	if err != nil {
		refuse(http.StatusUnprocessableEntity, err.Error())
		return
	}

	entries := make([]students.NewStudent, len(rows))
	for i := range rows {
		entries[i] = rows[i].entry()
	}

	added, rejected, err := s.roster.Add(r.Context(), group, entries)
	var valErr *students.ValidationError
	switch {
	case errors.As(err, &valErr):
		refuse(http.StatusConflict, valErr.Message)
		return
	case err != nil:
		httplog.RecordError(r, "save", err)
		http.Error(w, "Could not import students. Please try again.", http.StatusInternalServerError)
		return
	}

	report := rosterReport{Added: len(added)}
	for _, rj := range rejected {
		report.Rejected = append(report.Rejected, rejectedRow{
			Line:     rows[rj.Index].Line,
			Name:     entries[rj.Index].Name,
			Cells:    rows[rj.Index].Cells,
			Problems: rj.Problems(),
		})
	}

	if len(report.Rejected) > 0 {
		fixBytes, err := fixWorkbook(head, report.Rejected)
		if err != nil {
			httplog.RecordError(r, "export", err)
			http.Error(w, "Could not build the rows-to-fix file. Please try again.", http.StatusInternalServerError)
			return
		}
		report.FixFile = fixBytes
	}

	data.Imported = &report
	s.view(w, r, http.StatusOK, addStudentsView(data))
}
