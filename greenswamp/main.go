package main

import (
	"context"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Port            string
	TemplateDir     string
	StaticDir       string
	LogFile         string
	ReadTimeout     time.Duration
	ShutdownTimeout time.Duration
}

type PageData struct {
	Title  string
	Active string
	Year   int
}

type Site struct {
	logger    *log.Logger
	templates map[string]*template.Template
	config    Config
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func main() {
	cfg := Config{
		Port:            getEnv("PORT", "8080"),
		TemplateDir:     getEnv("TEMPLATE_DIR", "templates"),
		StaticDir:       getEnv("STATIC_DIR", "static"),
		LogFile:         getEnv("LOG_FILE", "server.log"),
		ReadTimeout:     5 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	}

	logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("open log file: %v", err)
	}
	defer logFile.Close()

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger := log.New(multiWriter, "", log.LstdFlags)

	site, err := newSite(cfg, logger)
	if err != nil {
		logger.Fatalf("load templates: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", site.route)

	if cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(cfg.StaticDir))
		mux.Handle("/static/", http.StripPrefix("/static/", fs))
	}

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           site.loggingMiddleware(mux),
		ReadHeaderTimeout: cfg.ReadTimeout,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Printf("server shutdown error: %v", err)
		}
		close(idleConnsClosed)
	}()

	logger.Printf("Server started on :%s", cfg.Port)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("listen and serve: %v", err)
	}

	<-idleConnsClosed
	logger.Println("Server gracefully stopped")
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func newSite(cfg Config, logger *log.Logger) (*Site, error) {
	funcMap := template.FuncMap{
		"eq": func(a, b string) bool { return a == b },
	}

	base := template.New("").Funcs(funcMap)

	sharedFiles, err := filepath.Glob(filepath.Join(cfg.TemplateDir, "shared/*.html"))
	if err != nil {
		return nil, err
	}
	if len(sharedFiles) > 0 {
		if _, err := base.ParseFiles(sharedFiles...); err != nil {
			return nil, err
		}
	}

	pageFiles := map[string]string{
		"index":   filepath.Join(cfg.TemplateDir, "pages/index.html"),
		"about":   filepath.Join(cfg.TemplateDir, "pages/about.html"),
		"contact": filepath.Join(cfg.TemplateDir, "pages/contact.html"),
		"privacy": filepath.Join(cfg.TemplateDir, "pages/privacy.html"),
		"tos":     filepath.Join(cfg.TemplateDir, "pages/tos.html"),
		"404":     filepath.Join(cfg.TemplateDir, "pages/404.html"),
	}

	templates := make(map[string]*template.Template)
	for key, pageFile := range pageFiles {
		cloned, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := cloned.ParseFiles(pageFile); err != nil {
			return nil, err
		}
		templates[key] = cloned
	}

	return &Site{
		logger:    logger,
		templates: templates,
		config:    cfg,
	}, nil
}

func (s *Site) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		ip := func() string {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				return host
			}
			return r.RemoteAddr
		}()
		s.logRequest(ip, r.Method, r.URL.RequestURI(), sw.status)
	})
}

func (s *Site) logRequest(ip, method, path string, status int) {
	s.logger.Printf("%s | IP: %s | %s %s | %d",
		time.Now().Format("2006-01-02 15:04:05"),
		ip,
		method,
		path,
		status,
	)
}

func (s *Site) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(strings.Trim(r.URL.Path, "/"), ".html")
	if path == "" {
		path = "index"
	}
	if path == "terms" {
		path = "tos"
	}

	key := path
	if _, ok := s.templates[key]; !ok {
		key = "404"
		if path != "404" {
			w.WriteHeader(http.StatusNotFound)
		}
	}

	data := PageData{
		Title:  buildTitle(key),
		Active: key,
		Year:   time.Now().Year(),
	}
	if key == "404" {
		data.Active = ""
	}

	s.renderPage(w, key, data)
}

func buildTitle(key string) string {
	switch key {
	case "index":
		return "Greenswamp - Campus Connection"
	case "about":
		return "Greenswamp - About"
	case "contact":
		return "Greenswamp - Contact"
	case "privacy":
		return "Greenswamp - Privacy Policy"
	case "tos":
		return "Greenswamp - Terms of Service"
	case "404":
		return "Greenswamp - Page Not Found"
	default:
		return "Greenswamp"
	}
}

func (s *Site) renderPage(w http.ResponseWriter, key string, data PageData) {
	tmpl, ok := s.templates[key]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	buf := new(strings.Builder)
	if err := tmpl.ExecuteTemplate(buf, key+".html", data); err != nil {
		http.Error(w, "template execution failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}
