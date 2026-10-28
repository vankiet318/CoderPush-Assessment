// Package web is the staff-facing HTTP interface.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

//go:embed templates/*.html
var templateFS embed.FS

// OutageControl is implemented by the simulated supplier only.
type OutageControl interface {
	SetOutage(fromBatch int)
	Outage() int
}

type Server struct {
	sync    *usecase.SyncService
	queries *usecase.QueryService
	issues  *usecase.IssueService
	outage  OutageControl // nil hides the simulator controls
	log     *slog.Logger
	pages   map[string]*template.Template
}

func NewServer(sync *usecase.SyncService, queries *usecase.QueryService, issues *usecase.IssueService, outage OutageControl, log *slog.Logger) (*Server, error) {
	s := &Server{sync: sync, queries: queries, issues: issues, outage: outage, log: log, pages: map[string]*template.Template{}}
	for _, page := range []string{"dashboard.html", "run.html", "issues.html", "products.html"} {
		t, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("POST /runs", s.startRun)
	mux.HandleFunc("GET /runs/{id}", s.runDetail)
	mux.HandleFunc("POST /runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("GET /issues", s.listIssues)
	mux.HandleFunc("POST /issues/{id}/close", s.closeIssue)
	mux.HandleFunc("GET /products", s.listProducts)
	mux.HandleFunc("POST /supplier/outage", s.setOutage)
	return mux
}

type dashboardView struct {
	usecase.Overview
	CanSimulate bool
	OutageFrom  int
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ov, err := s.queries.Overview(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	view := dashboardView{Overview: ov}
	if s.outage != nil {
		view.CanSimulate, view.OutageFrom = true, s.outage.Outage()
	}
	refresh := len(ov.Runs) > 0 && ov.Runs[0].IsRunning()
	s.render(w, r, "dashboard.html", "Updates", view, refresh)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.sync.StartRun(r.Context(), r.FormValue("feed"))
	if err != nil {
		redirect(w, r, "/", "err", err.Error())
		return
	}
	redirect(w, r, runPath(run.ID), "msg", "Update started.")
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	detail, err := s.queries.RunDetail(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "run.html", fmt.Sprintf("Update #%d", id), detail, detail.Run.IsRunning())
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.sync.ResumeRun(r.Context(), id); err != nil {
		redirect(w, r, runPath(id), "err", err.Error())
		return
	}
	redirect(w, r, runPath(id), "msg", "Update resumed from its last saved batch.")
}

type issuesView struct {
	Filter string
	Issues []domain.Issue
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	var status domain.IssueStatus
	switch filter {
	case "resolved":
		status = domain.IssueResolved
	case "all":
	default:
		filter, status = "open", domain.IssueOpen
	}
	issues, err := s.queries.Issues(r.Context(), status)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "issues.html", "Issues", issuesView{Filter: filter, Issues: issues}, false)
}

func (s *Server) closeIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/issues"
	}
	if _, err := s.issues.Close(r.Context(), id); err != nil {
		redirect(w, r, back, "err", err.Error())
		return
	}
	redirect(w, r, back, "msg", fmt.Sprintf("Issue #%d closed.", id))
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.queries.Products(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "products.html", "Catalog", products, false)
}

func (s *Server) setOutage(w http.ResponseWriter, r *http.Request) {
	if s.outage == nil {
		http.NotFound(w, r)
		return
	}
	from, err := strconv.Atoi(r.FormValue("from"))
	if err != nil || from < 0 {
		redirect(w, r, "/", "err", "Outage batch must be a whole number (0 = online).")
		return
	}
	s.outage.SetOutage(from)
	msg := "Supplier is online."
	if from > 0 {
		msg = fmt.Sprintf("Supplier will fail from batch %d.", from)
	}
	redirect(w, r, "/", "msg", msg)
}

// ---- helpers ----

type page struct {
	Title   string
	Flash   string
	Error   string
	Refresh bool
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any, refresh bool) {
	q := r.URL.Query()
	var buf bytes.Buffer
	err := s.pages[name].ExecuteTemplate(&buf, "layout", page{
		Title: title, Flash: q.Get("msg"), Error: q.Get("err"), Refresh: refresh, Data: data,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func runPath(id int64) string { return fmt.Sprintf("/runs/%d", id) }

func redirect(w http.ResponseWriter, r *http.Request, path, key, value string) {
	http.Redirect(w, r, path+"?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

var templateFuncs = template.FuncMap{
	"price": domain.FormatPrice,
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
	"css": func(v any) string { return strings.ToLower(fmt.Sprint(v)) },
	"orDash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
}
