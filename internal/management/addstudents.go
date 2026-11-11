package management

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"projecthermes/internal/logging"
	"projecthermes/internal/students"
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
	FixFile  template.URL // data: URL of the fix workbook; "" when nothing was rejected
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
			return nil, nil, &students.ValidationError{Message: "Could not read this file. Upload an .xlsx or .csv roster."}
		}
		defer f.Close()

		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, nil, &students.ValidationError{Message: "Could not read this file. Upload an .xlsx or .csv roster."}
		}
		rows, err := f.GetRows(sheets[0])
		if err != nil {
			return nil, nil, &students.ValidationError{Message: "Could not read this file. Upload an .xlsx or .csv roster."}
		}
		rawRows = rows
	case ".csv":
		r := csv.NewReader(file)
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil {
			return nil, nil, &students.ValidationError{Message: "Could not read this file. Upload an .xlsx or .csv roster."}
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

func registerAddStudentRoutes(mux *http.ServeMux, roster *students.Store, staff func(context.Context) string, page *template.Template) {
	mux.HandleFunc("GET /management/students/new", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		data := addStudentsData{Staff: staff(r.Context()), Groups: students.GroupOptions()}
		if rawID := r.URL.Query().Get("added"); rawID != "" {
			if id, err := strconv.ParseUint(rawID, 10, 64); err == nil {
				data.Added, err = roster.Get(ctx, id)
				if err != nil {
					logging.RecordError(r, "load", err)
					http.Error(w, "Could not load student. Please try again.", http.StatusInternalServerError)
					return
				}
				if data.Added != nil {
					data.AddGroup = data.Added.Group
				}
			}
		}

		render(w, r, page, http.StatusOK, "add-students.html", data)
	})

	mux.HandleFunc("POST /management/students", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := r.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "This value is too large.", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "Could not read this field. Please try again.", http.StatusBadRequest)
			}
			return
		}

		entry := students.NewStudent{Name: r.PostForm.Get("name"), Email: r.PostForm.Get("email"), Pin: r.PostForm.Get("pin")}
		data := addStudentsData{Staff: staff(r.Context()), Groups: students.GroupOptions(), Entry: entry, AddGroup: r.PostForm.Get("group")}
		group, err := students.ParseGroup(data.AddGroup, true)
		if err != nil {
			data.AddError = err.Error()
			render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
			return
		}
		added, rejected, err := roster.Add(ctx, group, []students.NewStudent{entry})

		var valErr *students.ValidationError
		switch {
		case errors.As(err, &valErr):
			data.AddError = valErr.Message
			render(w, r, page, http.StatusConflict, "add-students.html", data)
		case err != nil:
			logging.RecordError(r, "save", err)
			http.Error(w, "Could not add the student. Please try again.", http.StatusInternalServerError)
		case len(rejected) > 0:
			data.Errors = rejected[0]
			render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
		default:
			http.Redirect(w, r, "/management/students/new?added="+strconv.FormatUint(added[0].ID, 10), http.StatusSeeOther)
		}
	})

	mux.HandleFunc("POST /management/students/import", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		data := addStudentsData{Staff: staff(r.Context()), Groups: students.GroupOptions()}

		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				data.ImportError = "The file is larger than 2 MB."
				render(w, r, page, http.StatusRequestEntityTooLarge, "add-students.html", data)
			} else {
				data.ImportError = "Choose an .xlsx or .csv file to upload."
				render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
			}
			return
		}

		data.ImportGroup = r.PostForm.Get("group")
		group, err := students.ParseGroup(data.ImportGroup, true)
		if err != nil {
			data.ImportError = err.Error()
			render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
			return
		}
		data.ImportGroup = group

		file, header, err := r.FormFile("roster")
		if err != nil {
			data.ImportError = "Choose an .xlsx or .csv file to upload."
			render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
			return
		}
		defer file.Close()

		head, rows, err := readRoster(header.Filename, file)
		if err != nil {
			var valErr *students.ValidationError
			if errors.As(err, &valErr) {
				data.ImportError = valErr.Message
				render(w, r, page, http.StatusUnprocessableEntity, "add-students.html", data)
				return
			}
			logging.RecordError(r, "import", err)
			http.Error(w, "Could not read this file. Please try again.", http.StatusInternalServerError)
			return
		}

		entries := make([]students.NewStudent, len(rows))
		for i := range rows {
			entries[i] = rows[i].entry()
		}

		added, rejected, err := roster.Add(ctx, group, entries)
		var valErr *students.ValidationError
		switch {
		case errors.As(err, &valErr):
			data.ImportError = valErr.Message
			render(w, r, page, http.StatusConflict, "add-students.html", data)
			return
		case err != nil:
			logging.RecordError(r, "save", err)
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
				logging.RecordError(r, "export", err)
				http.Error(w, "Could not build the rows-to-fix file. Please try again.", http.StatusInternalServerError)
				return
			}
			report.FixFile = template.URL("data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64," + base64.StdEncoding.EncodeToString(fixBytes))
		}

		data.Imported = &report
		render(w, r, page, http.StatusOK, "add-students.html", data)
	})
}
