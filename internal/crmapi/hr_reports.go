package crmapi

// HR portal — reports and CSV export (HB4, D16). Super admin only.

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type hrReportPersonJSON struct {
	EmployeeID  uuid.UUID `json:"employeeId"`
	DisplayName *string   `json:"displayName"`
	Position    *string   `json:"position"`
	IsTracked   bool      `json:"isTracked"`
}

type hrMonthReportRowJSON struct {
	Person       hrReportPersonJSON `json:"person"`
	Totals       hrTotals           `json:"totals"`
	MakeupOwed   float64            `json:"makeupOwed"`
	VacationLeft float64            `json:"vacationLeft"`
	VacationOver float64            `json:"vacationOver"`
}

type hrYearMonthJSON struct {
	Month       string  `json:"month"`
	PlannedDays float64 `json:"plannedDays"`
	AbsenceDays float64 `json:"absenceDays"`
	IsFuture    bool    `json:"isFuture"`
}

type hrYearReportRowJSON struct {
	Person   hrReportPersonJSON `json:"person"`
	Months   []hrYearMonthJSON  `json:"months"`
	Totals   hrTotals           `json:"totals"`
	Vacation hrVacationBalance  `json:"vacation"`
}

type hrReportJSON struct {
	Office   hrOfficeJSON           `json:"office"`
	Period   string                 `json:"period"`
	Month    *string                `json:"month,omitempty"`
	Year     int                    `json:"year"`
	Today    string                 `json:"today"`
	Rows     []hrMonthReportRowJSON `json:"rows,omitempty"`
	YearRows []hrYearReportRowJSON  `json:"yearRows,omitempty"`
}

// hrAbsenceTotal is every absence day of the period (make-up days are work, not absence).
func hrAbsenceTotal(t hrTotals) float64 {
	return hrRoundHalf(t.Vacation + t.Sick + t.DayOffUnpaid + t.DayOffMakeup + t.Remote + t.Occasional + t.Other)
}

type hrReportQuery struct {
	OfficeID uuid.UUID
	Period   string
	From     time.Time
	To       time.Time
	Year     int
}

func (s *Server) hrParseReportQuery(w http.ResponseWriter, r *http.Request, today time.Time) (hrReportQuery, bool) {
	q := hrReportQuery{Period: r.URL.Query().Get("period")}
	fields := map[string]string{}
	officeID, ok := hrQueryUUID(r, "officeId")
	if !ok || officeID == nil {
		fields["officeId"] = "validation_error"
	} else {
		q.OfficeID = *officeID
	}
	switch q.Period {
	case "", "month":
		q.Period = "month"
		month := hrMonthStart(today)
		if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
			m, ok := hrParseDate(raw + "-01")
			if !ok {
				fields["month"] = "validation_error"
			}
			month = m
		}
		q.From, q.To, q.Year = month, hrMonthEnd(month), month.Year()
	case "year":
		q.Year = today.Year()
		if raw := strings.TrimSpace(r.URL.Query().Get("year")); raw != "" {
			y, err := strconv.Atoi(raw)
			if err != nil || y < 2000 || y > 2100 {
				fields["year"] = "validation_error"
			}
			q.Year = y
		}
		q.From = time.Date(q.Year, 1, 1, 0, 0, 0, 0, time.UTC)
		q.To = time.Date(q.Year, 12, 31, 0, 0, 0, 0, time.UTC)
	default:
		fields["period"] = "validation_error"
	}
	if len(fields) > 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Invalid report request", fields)
		return q, false
	}
	return q, true
}

func (s *Server) hrBuildReport(ctx context.Context, q hrReportQuery) (*hrReportJSON, error) {
	office, err := hrLoadOffice(ctx, s.pool, q.OfficeID)
	if err != nil || office == nil {
		return nil, err
	}
	today := hrToday(office.TimeZone)
	rows, err := hrLoadDays(ctx, s.pool, &q.OfficeID, q.From, q.To, nil)
	if err != nil {
		return nil, err
	}
	employees, err := hrListEmployees(ctx, s.pool, nil)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]hrEmployee{}
	for _, e := range employees {
		byID[e.ID] = e
	}
	// People with at least one tracked day in this office during the period.
	present := map[uuid.UUID]bool{}
	for _, d := range rows {
		present[d.EmployeeID] = true
	}
	ids := []uuid.UUID{}
	for id := range present {
		if _, ok := byID[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := byID[ids[i]].DisplayName, byID[ids[j]].DisplayName
		if a == nil || b == nil {
			return a != nil
		}
		return strings.ToLower(*a) < strings.ToLower(*b)
	})
	out := &hrReportJSON{Office: office.json(), Period: q.Period, Year: q.Year, Today: hrFormatDate(today)}
	person := func(e hrEmployee) hrReportPersonJSON {
		return hrReportPersonJSON{EmployeeID: e.ID, DisplayName: e.DisplayName, Position: e.Position, IsTracked: e.isTracked(hrToday(e.TimeZone))}
	}
	if q.Period == "month" {
		m := q.From.Format("2006-01")
		out.Month = &m
		totals := hrAggregate(rows)
		out.Rows = []hrMonthReportRowJSON{}
		for _, id := range ids {
			e := byID[id]
			b, err := hrLoadBalances(ctx, s.pool, e, q.Year)
			if err != nil {
				return nil, err
			}
			out.Rows = append(out.Rows, hrMonthReportRowJSON{Person: person(e), Totals: totals[id], MakeupOwed: b.MakeupOwed,
				VacationLeft: b.Vacation.Left, VacationOver: b.Vacation.Over})
		}
		return out, nil
	}
	perMonth := map[uuid.UUID]map[int][]hrDayRow{}
	for _, d := range rows {
		if perMonth[d.EmployeeID] == nil {
			perMonth[d.EmployeeID] = map[int][]hrDayRow{}
		}
		perMonth[d.EmployeeID][int(d.Day.Month())] = append(perMonth[d.EmployeeID][int(d.Day.Month())], d)
	}
	totals := hrAggregate(rows)
	currentMonth := hrMonthStart(today)
	out.YearRows = []hrYearReportRowJSON{}
	for _, id := range ids {
		e := byID[id]
		row := hrYearReportRowJSON{Person: person(e), Totals: totals[id]}
		for month := 1; month <= 12; month++ {
			start := time.Date(q.Year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
			t := hrAggregate(perMonth[id][month])[id]
			row.Months = append(row.Months, hrYearMonthJSON{Month: start.Format("2006-01"), PlannedDays: t.PlannedDays,
				AbsenceDays: hrAbsenceTotal(t), IsFuture: start.After(currentMonth)})
		}
		limit, err := hrLeaveLimit(ctx, s.pool, e, q.Year)
		if err != nil {
			return nil, err
		}
		row.Vacation = hrBalance(q.Year, limit, row.Totals.Vacation)
		out.YearRows = append(out.YearRows, row)
	}
	return out, nil
}

func (s *Server) handleHRReport(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	q, ok := s.hrParseReportQuery(w, r, v.today)
	if !ok {
		return
	}
	report, err := s.hrBuildReport(r.Context(), q)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if report == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Office not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// CSV headers per UI language (uk default). The CRM passes its active locale.
var hrReportHeaders = map[string]map[string]string{
	"uk": {"name": "Працівник", "position": "Посада", "norm": "Норма, дні", "plannedDays": "Заплановано, дні", "plannedHours": "Заплановано, год",
		"vacation": "Відпустка", "sick": "Лікарняний", "dayOffUnpaid": "Вихідний (неоплачуваний)", "dayOffMakeup": "Вихідний з відпрацюванням",
		"makeupDone": "Відпрацьовано", "makeupOwed": "Борг відпрацювання", "remote": "Віддалено", "occasional": "Відпустка за обставинами",
		"other": "Інше", "vacationLeft": "Залишок відпустки (рік)", "vacationOver": "Понад ліміт", "absence": "Відсутність, дні",
		"vacationLimit": "Ліміт відпустки", "vacationUsed": "Використано відпустки"},
	"pl": {"name": "Pracownik", "position": "Stanowisko", "norm": "Norma, dni", "plannedDays": "Zaplanowane dni", "plannedHours": "Zaplanowane godziny",
		"vacation": "Urlop", "sick": "Chorobowe", "dayOffUnpaid": "Dzień wolny (bezpłatny)", "dayOffMakeup": "Dzień wolny do odpracowania",
		"makeupDone": "Odpracowane", "makeupOwed": "Do odpracowania", "remote": "Zdalnie", "occasional": "Urlop okolicznościowy",
		"other": "Inne", "vacationLeft": "Pozostały urlop (rok)", "vacationOver": "Ponad limit", "absence": "Nieobecność, dni",
		"vacationLimit": "Limit urlopu", "vacationUsed": "Wykorzystany urlop"},
	"en": {"name": "Employee", "position": "Position", "norm": "Norm days", "plannedDays": "Planned days", "plannedHours": "Planned hours",
		"vacation": "Vacation", "sick": "Sick leave", "dayOffUnpaid": "Day off (unpaid)", "dayOffMakeup": "Day off — to make up",
		"makeupDone": "Make-up done", "makeupOwed": "Make-up owed", "remote": "Remote", "occasional": "Occasional leave",
		"other": "Other", "vacationLeft": "Vacation left (year)", "vacationOver": "Over limit", "absence": "Absence days",
		"vacationLimit": "Vacation limit", "vacationUsed": "Vacation used"},
}

func hrNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func hrStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func hrTotalsCells(t hrTotals) []string {
	return []string{hrNum(t.Vacation), hrNum(t.Sick), hrNum(t.DayOffUnpaid), hrNum(t.DayOffMakeup), hrNum(t.MakeupDone)}
}

func hrReportCSV(report *hrReportJSON, lang string) ([]byte, error) {
	h := hrReportHeaders[lang]
	if h == nil {
		h = hrReportHeaders["uk"]
	}
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // UTF-8 BOM so Excel opens Cyrillic / Polish letters correctly
	cw := csv.NewWriter(&buf)
	if report.Period == "month" {
		_ = cw.Write([]string{h["name"], h["position"], h["norm"], h["plannedDays"], h["plannedHours"], h["vacation"], h["sick"],
			h["dayOffUnpaid"], h["dayOffMakeup"], h["makeupDone"], h["makeupOwed"], h["remote"], h["occasional"], h["other"], h["vacationLeft"], h["vacationOver"]})
		for _, row := range report.Rows {
			t := row.Totals
			record := []string{hrStr(row.Person.DisplayName), hrStr(row.Person.Position), hrNum(t.NormDays), hrNum(t.PlannedDays), hrNum(t.PlannedHours)}
			record = append(record, hrTotalsCells(t)...)
			record = append(record, hrNum(row.MakeupOwed), hrNum(t.Remote), hrNum(t.Occasional), hrNum(t.Other), hrNum(row.VacationLeft), hrNum(row.VacationOver))
			_ = cw.Write(record)
		}
	} else {
		header := []string{h["name"], h["position"]}
		for month := 1; month <= 12; month++ {
			m := fmt.Sprintf("%d-%02d", report.Year, month)
			header = append(header, m+" · "+h["plannedDays"], m+" · "+h["absence"])
		}
		header = append(header, h["norm"], h["plannedDays"], h["plannedHours"], h["vacation"], h["sick"], h["dayOffUnpaid"], h["dayOffMakeup"],
			h["makeupDone"], h["remote"], h["occasional"], h["other"], h["vacationLimit"], h["vacationUsed"], h["vacationOver"])
		_ = cw.Write(header)
		for _, row := range report.YearRows {
			record := []string{hrStr(row.Person.DisplayName), hrStr(row.Person.Position)}
			for _, m := range row.Months {
				if m.IsFuture {
					record = append(record, "", "")
					continue
				}
				record = append(record, hrNum(m.PlannedDays), hrNum(m.AbsenceDays))
			}
			t := row.Totals
			record = append(record, hrNum(t.NormDays), hrNum(t.PlannedDays), hrNum(t.PlannedHours))
			record = append(record, hrTotalsCells(t)...)
			record = append(record, hrNum(t.Remote), hrNum(t.Occasional), hrNum(t.Other), hrNum(row.Vacation.Limit), hrNum(row.Vacation.Used), hrNum(row.Vacation.Over))
			_ = cw.Write(record)
		}
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// GET /v1/hr/reports/export — CSV only. XLSX needs a new dependency: TODO(hr) owner question.
func (s *Server) handleHRReportExport(w http.ResponseWriter, r *http.Request) {
	v, ok := s.hrRequireAdmin(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "csv" {
		s.writeError(w, r, http.StatusBadRequest, "validation_error", "Only CSV export is available", map[string]string{"format": "validation_error"})
		return
	}
	q, ok := s.hrParseReportQuery(w, r, v.today)
	if !ok {
		return
	}
	report, err := s.hrBuildReport(r.Context(), q)
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	if report == nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "Office not found", nil)
		return
	}
	data, err := hrReportCSV(report, r.URL.Query().Get("lang"))
	if err != nil {
		s.hrFail(w, r, err)
		return
	}
	name := "kolss-hr-" + report.Office.Code + "-" + strconv.Itoa(report.Year)
	if report.Month != nil {
		name = "kolss-hr-" + report.Office.Code + "-" + *report.Month
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
