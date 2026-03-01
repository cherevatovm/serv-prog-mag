package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	rootDir  string
	listener net.Listener
	ctx      context.Context
	wg       sync.WaitGroup

	logFile *os.File
	cancel  context.CancelFunc
	mu      sync.Mutex
}

type Request struct {
	Method  string
	Path    string
	Version string
}

func main() {
	rootDir, err := os.Getwd()
	if err != nil {
		fmt.Printf("Failed to get current directory: %v\n", err)
		return
	}

	logFile, err := os.OpenFile("server.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("Failed to create log file:", err)
		return
	}
	defer logFile.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		rootDir: rootDir,
		logFile: logFile,
		ctx:     ctx,
		cancel:  cancel,
	}

	go server.Start(":8080")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\nStopping server")

	server.Shutdown()
	fmt.Println("Server gracefully stopped")
}

func (s *Server) Start(addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("Error starting server:", err)
		return
	}
	s.listener = listener

	fmt.Printf("Server started on %s\n", addr)
	fmt.Printf("Root directory: %s\n", s.rootDir)

	for {
		select {
		case <-s.ctx.Done():
			fmt.Println("Server stops accepting new connections")
			return
		default:
		}

		if tcpListener, ok := listener.(*net.TCPListener); ok {
			tcpListener.SetDeadline(time.Now().Add(time.Second))
		}

		conn, err := listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue
			}
			if s.ctx.Err() != nil {
				return
			}
			fmt.Printf("Error accepting connection: %v\n", err)
			continue
		}

		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

		s.wg.Go(func() {
			s.handleConnection(conn)
		})
	}
}

func (s *Server) Shutdown() {
	s.cancel()
	if s.listener != nil {
		s.listener.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		fmt.Println("All requests compeleted")
	case <-time.After(10 * time.Second):
		fmt.Println("Timeout waiting for requests completion")
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	reader := bufio.NewReader(conn)

	req, err := parseRequest(reader)
	if err != nil {
		s.sendError(conn, 400, "Bad Request")
		s.logRequest(clientIP, "N/A", 400)
		return
	}

	if req.Method != "GET" {
		s.sendError(conn, 405, "Method Not Allowed")
		s.logRequest(clientIP, req.Path, 405)
		return
	}

	status := s.handleGet(conn, req)
	s.logRequest(clientIP, req.Path, status)
}

func parseRequest(reader *bufio.Reader) (*Request, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}

	line = strings.TrimSpace(line)
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return nil, fmt.Errorf("Invalid request format")
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	return &Request{
		Method:  parts[0],
		Path:    parts[1],
		Version: parts[2],
	}, nil
}

func (s *Server) handleGet(conn net.Conn, req *Request) int {
	decodedPath, err := url.PathUnescape(req.Path)
	if err != nil {
		s.sendError(conn, 400, "Bad Request")
		return 400
	}
	cleanPath := filepath.Clean(decodedPath)
	fullPath := filepath.Join(s.rootDir, cleanPath)

	absRoot, err := filepath.Abs(s.rootDir)
	if err != nil {
		s.sendError(conn, 500, "Internal Server Error")
		return 500
	}

	absFile, err := filepath.Abs(fullPath)
	if err != nil {
		s.sendError(conn, 500, "Internal Server Error")
		return 500
	}

	if !strings.HasPrefix(absFile, absRoot) {
		s.sendError(conn, 403, "Forbidden")
		return 403
	}

	fileInfo, err := os.Stat(absFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.sendError(conn, 404, "Not Found")
			return 404
		}
		s.sendError(conn, 500, "Internal Server Error")
		return 500
	}

	if fileInfo.IsDir() {
		indexPath := filepath.Join(absFile, "index.html")
		if indexInfo, err := os.Stat(indexPath); err == nil && !indexInfo.IsDir() {
			return s.sendFile(conn, indexPath, indexInfo)
		}
		return s.sendDirListing(conn, absFile, req.Path)
	}

	return s.sendFile(conn, absFile, fileInfo)
}

func (s *Server) sendFile(conn net.Conn, filePath string, fileInfo os.FileInfo) int {
	file, err := os.Open(filePath)
	if err != nil {
		s.sendError(conn, 500, "Internal Server Error")
		return 500
	}
	defer file.Close()

	contentType := mime.TypeByExtension(filepath.Ext(filePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	header := fmt.Sprintf(
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: %s\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n"+
			"Server: CustomGoServer/1.0\r\n\r\n",
		contentType,
		fileInfo.Size(),
	)

	if _, err := conn.Write([]byte(header)); err != nil {
		return 500
	}

	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(conn, file, buf); err != nil {
		return 500
	}

	return 200
}

func (s *Server) sendDirListing(conn net.Conn, dirPath, reqPath string) int {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		s.sendError(conn, 500, "Internal Server Error")
		return 500
	}

	type fileEntry struct {
		name  string
		size  int64
		isDir bool
	}

	var dirs, regFiles []fileEntry
	for _, f := range files {
		info, _ := f.Info()
		entry := fileEntry{
			name:  f.Name(),
			isDir: f.IsDir(),
		}
		if info != nil {
			entry.size = info.Size()
		}

		if f.IsDir() {
			dirs = append(dirs, entry)
		} else {
			regFiles = append(regFiles, entry)
		}
	}

	var html strings.Builder
	html.WriteString(`<!DOCTYPE html>
		<html>
		<head>
		<meta charset="utf-8">
		<title>Index of ` + reqPath + `</title>
		<style>
		body {
			font-family: "Segoe UI", sans-serif;
			background: #f6f8fa;
			margin: 40px;
		}
		h1 {
			font-weight: 500;
		}
		table {
			border-collapse: collapse;
			width: 100%;
			background: white;
			border-radius: 8px;
			overflow: hidden;
			box-shadow: 0 2px 8px rgba(0,0,0,0.05);
		}
		th, td {
			padding: 10px 14px;
			text-align: left;
		}
		th {
			background: #f0f2f5;
			font-weight: 600;
		}
		tr:nth-child(even) {
			background: #fafafa;
		}
		a {
			text-decoration: none;
			color: #0366d6;
		}
		a:hover {
			text-decoration: underline;
		}
		.size {
			color: #666;
			font-size: 0.9em;
		}
		</style>
		</head>
		<body>
		<h1>Contents of ` + reqPath + `</h1>
		<table>
		<tr><th>Name</th><th>Size</th></tr>`)

	if reqPath != "/" {
		html.WriteString(`<tr><td><a href="../">⬅ ..</a></td><td></td></tr>`)
	}
	for _, d := range dirs {
		fmt.Fprintf(&html, `<tr><td>🖿 <a href="%s/">%s/</a></td><td class="size">—</td></tr>`,
			d.name, d.name)
	}
	for _, f := range regFiles {
		fmt.Fprintf(&html, `<tr><td>🗎 <a href="%s">%s</a></td><td class="size">%d bytes</td></tr>`,
			f.name, f.name, f.size)
	}
	html.WriteString(`</table></body></html>`)

	body := html.String()
	response := fmt.Sprintf(
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: text/html; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n"+
			"Server: CustomGoServer/1.0\r\n\r\n"+
			"%s",
		len(body), body,
	)

	if _, err := conn.Write([]byte(response)); err != nil {
		return 500
	}

	return 200
}

func (s *Server) sendError(conn net.Conn, code int, text string) {
	body := fmt.Sprintf("<!DOCTYPE html>\n<html>\n<head>\n<title>%d %s</title>\n</head>\n<body>\n<h1>%d %s</h1>\n</body>\n</html>",
		code, text, code, text)

	response := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\n"+
			"Content-Type: text/html; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n"+
			"Server: CustomGoServer/1.0\r\n\r\n"+
			"%s",
		code, text, len(body), body,
	)

	conn.Write([]byte(response))
}

func (s *Server) logRequest(ip, path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timestamp := time.Now().Format("2005-01-02 15:04:05")
	fmt.Printf("%s | IP: %-15s | Path: %-30s | %d\033[0m\n",
		timestamp, ip, path, status)

	entry := fmt.Sprintf("%s | %s | %s | %d\n",
		timestamp, ip, path, status)

	if _, err := s.logFile.WriteString(entry); err != nil {
		fmt.Printf("Error writing to log: %v\n", err)
	}
}
