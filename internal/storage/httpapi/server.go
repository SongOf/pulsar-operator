// Package httpapi serves the storage console without owning node devices or
// reconciliation. Its backend implements either demo state or Kubernetes CRUD.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const MaxBodyBytes = 65536
const contentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'self'"

type Server struct {
	Mode    string
	Token   string
	Backend model.Backend
	// Static includes the static/ prefix (for example the embedded web.FS).
	Static fs.FS
	Logger *slog.Logger
}

// ValidateConfig is also used before the CLI opens state or binds a socket.
func ValidateConfig(mode, host, token string) error {
	if mode != "demo" && mode != "cluster" {
		return fmt.Errorf("MODE must be demo or cluster")
	}
	if mode == "cluster" && utf8.RuneCountInString(token) < 24 {
		return fmt.Errorf("cluster mode requires API_TOKEN of at least 24 characters")
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" && utf8.RuneCountInString(token) < 24 {
		return fmt.Errorf("监听外部地址需要至少 24 字符的 API_TOKEN")
	}
	return nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The panic or error may contain upstream credentials or user input.
			s.logger().Error("storage request panicked", "type", fmt.Sprintf("%T", recovered))
			s.sendError(w, errors.New("request panic"))
		}
	}()
	if err := validateHeaders(r); err != nil {
		s.sendError(w, err)
		return
	}
	if hasControl(r.URL.Path) || hasControl(r.URL.RawQuery) {
		s.sendError(w, model.NewProblem(400, "URL 路径包含控制字符"))
		return
	}
	if r.URL.RawQuery != "" && strings.Count(r.URL.RawQuery, "&") >= 100 {
		s.sendError(w, model.NewProblem(400, "无效 URL 或查询参数过多"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.sendError(w, model.NewProblem(400, "无效 URL 或查询参数过多"))
		return
	}
	// Match literal API paths and decode only the resource identifier once.
	route := r.URL.EscapedPath()
	if r.Method == http.MethodGet && (route == "/" || strings.HasPrefix(route, "/static/")) {
		s.serveStatic(w, r.URL.Path)
		return
	}
	if route != "/api/config" && route != "/healthz" && s.Token != "" {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
			s.sendError(w, model.NewProblem(401, "需要有效的管理 Token"))
			return
		}
	}
	body := model.Object{}
	if r.Method == http.MethodPost {
		body, err = readObject(r)
		if err != nil {
			s.sendError(w, err)
			return
		}
	}
	result, err := s.route(r.Context(), r.Method, route, query, body)
	if err != nil {
		s.sendError(w, err)
		return
	}
	s.sendJSON(w, 200, result)
}

func validateHeaders(r *http.Request) error {
	if conn, ok := r.Context().Value(framingKey{}).(*framedConn); ok && conn.framingError != nil {
		return conn.framingError
	}
	if len(r.Header.Values("Content-Length")) > 1 {
		return model.NewProblem(400, "不接受重复的 Content-Length")
	}
	if len(r.TransferEncoding) > 0 || len(r.Header.Values("Transfer-Encoding")) > 0 {
		return model.NewProblem(400, "不接受 Transfer-Encoding；请发送明确的 Content-Length")
	}
	if len(r.Header.Values("Authorization")) > 1 {
		return model.NewProblem(400, "不接受重复的 Authorization")
	}
	return nil
}

func (s *Server) route(ctx context.Context, method, route string, query url.Values, body model.Object) (any, error) {
	if method == http.MethodGet {
		switch route {
		case "/api/config":
			return model.Object{"mode": s.Mode, "tokenRequired": s.Token != ""}, nil
		case "/healthz":
			return model.Object{"status": "ok", "mode": s.Mode}, nil
		case "/api/overview":
			return s.Backend.Overview(ctx)
		case "/api/nodes", "/api/disks", "/api/pools", "/api/volumes", "/api/events":
			opts, err := listOptions(query)
			if err != nil {
				return nil, err
			}
			kind := strings.TrimPrefix(route, "/api/")
			if kind == "events" {
				return s.Backend.Events(ctx, opts)
			}
			return s.Backend.List(ctx, kind, opts)
		}
	}
	if method == http.MethodPost {
		switch route {
		case "/api/pools/plan":
			return s.Backend.Plan(ctx, body)
		case "/api/pools":
			id, err := model.Identifier(body["planId"])
			if err != nil {
				return nil, err
			}
			return s.Backend.CreatePool(ctx, id, model.String(body["confirmation"]))
		case "/api/volumes":
			if _, err := model.Identifier(body["poolId"]); err != nil {
				return nil, err
			}
			return s.Backend.CreateVolume(ctx, body)
		}
		parts := strings.Split(route, "/")
		if len(parts) == 5 && parts[1] == "api" && parts[2] == "volumes" && parts[4] == "expand" {
			id, err := decodeIdentifier(parts[3])
			if err != nil {
				return nil, err
			}
			return s.Backend.Expand(ctx, id, body)
		}
	}
	if method == http.MethodDelete {
		parts := strings.Split(route, "/")
		if len(parts) == 4 && parts[1] == "api" && parts[2] == "volumes" {
			id, err := decodeIdentifier(parts[3])
			if err != nil {
				return nil, err
			}
			uid := ""
			if values, exists := query["uid"]; s.Mode == "cluster" && exists {
				if len(values) != 1 {
					return nil, model.NewProblem(400, "只能提供一个 PVC UID")
				}
				uid, err = model.Identifier(values[0])
				if err != nil {
					return nil, err
				}
			}
			return s.Backend.Release(ctx, id, uid)
		}
	}
	return nil, model.NewProblem(404, "接口不存在")
}

func decodeIdentifier(encoded string) (string, error) {
	value, err := url.PathUnescape(encoded)
	if err != nil {
		return "", model.NewProblem(400, "无效资源 ID")
	}
	return model.Identifier(value)
}

func listOptions(query url.Values) (model.ListOptions, error) {
	limit, offset := 50, 0
	var err error
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil {
			return model.ListOptions{}, model.NewProblem(400, "分页参数须为整数")
		}
	}
	if query.Has("offset") {
		offset, err = strconv.Atoi(query.Get("offset"))
		if err != nil {
			return model.ListOptions{}, model.NewProblem(400, "分页参数须为整数")
		}
	}
	if limit < 1 || limit > 500 || offset < 0 {
		return model.ListOptions{}, model.NewProblem(400, "limit 须为 1–500，offset 不得为负数")
	}
	return model.ListOptions{Limit: limit, Offset: offset, Node: truncate(query.Get("node")), State: truncate(query.Get("state")), Query: truncate(query.Get("q"))}, nil
}

func truncate(value string) string {
	chars := []rune(value)
	if len(chars) > 256 {
		chars = chars[:256]
	}
	return string(chars)
}
func hasControl(value string) bool {
	for _, char := range value {
		if char < 32 || char == 127 {
			return true
		}
	}
	return false
}

func (s *Server) serveStatic(w http.ResponseWriter, route string) {
	name := "index.html"
	if route != "/" {
		name = strings.TrimPrefix(route, "/static/")
	}
	if s.Static == nil || !fs.ValidPath(name) || strings.Contains(name, "\\") || hasControl(name) {
		s.sendError(w, model.NewProblem(404, "文件不存在"))
		return
	}
	data, err := fs.ReadFile(s.Static, "static/"+name)
	if err != nil {
		s.sendError(w, model.NewProblem(404, "文件不存在"))
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if strings.HasSuffix(name, ".js") {
		contentType = "application/javascript; charset=utf-8"
	}
	s.send(w, 200, contentType, data)
}

func (s *Server) sendError(w http.ResponseWriter, err error) {
	status, detail := 500, "服务内部错误，请查看服务日志"
	var problem *model.Problem
	var apiStatus apierrors.APIStatus
	var networkError net.Error
	switch {
	case errors.As(err, &problem):
		status, detail = problem.Status, problem.Detail
	case errors.As(err, &apiStatus):
		status, detail = int(apiStatus.Status().Code), apiStatus.Status().Message
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()):
		status, detail = 408, "读取请求超时"
	default:
		s.logger().Error("storage request failed", "type", fmt.Sprintf("%T", err))
	}
	if status < 400 || status > 599 {
		status, detail = 500, "服务内部错误，请查看服务日志"
	}
	s.sendJSON(w, status, model.Object{"detail": detail})
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
func (s *Server) sendJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status, data = 500, []byte(`{"detail":"服务内部错误，请查看服务日志"}`)
	}
	s.send(w, status, "application/json; charset=utf-8", data)
}
func (s *Server) send(w http.ResponseWriter, status int, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func readObject(r *http.Request) (model.Object, error) {
	if strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])) != "application/json" {
		return nil, model.NewProblem(415, "需要 application/json")
	}
	rawLength := strings.TrimSpace(r.Header.Get("Content-Length"))
	if rawLength == "" {
		// net/http exposes framing as ContentLength; an in-memory request may
		// populate this field without explicitly materializing its header.
		if r.ContentLength <= 0 {
			return nil, model.NewProblem(411, "需要 Content-Length")
		}
		rawLength = strconv.FormatInt(r.ContentLength, 10)
	}
	for _, char := range rawLength {
		if char < '0' || char > '9' {
			return nil, model.NewProblem(400, "无效 Content-Length")
		}
	}
	length, err := strconv.ParseUint(rawLength, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, model.NewProblem(413, "请求体为空或超过 64 KiB")
		}
		return nil, model.NewProblem(400, "无效 Content-Length")
	}
	if length == 0 || length > MaxBodyBytes {
		return nil, model.NewProblem(413, "请求体为空或超过 64 KiB")
	}
	if r.Body == nil {
		return nil, model.NewProblem(400, "请求体长度不足")
	}
	data := make([]byte, int(length))
	if _, err = io.ReadFull(r.Body, data); err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, err
		}
		return nil, model.NewProblem(400, "请求体长度不足")
	}
	if !utf8.Valid(data) {
		return nil, model.NewProblem(400, "无效 JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	value, err := jsonValue(decoder, 0)
	if err != nil {
		return nil, model.NewProblem(400, "无效 JSON")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, model.NewProblem(400, "无效 JSON")
	}
	body, ok := value.(map[string]any)
	if !ok {
		return nil, model.NewProblem(400, "请求体必须为 JSON 对象")
	}
	return body, nil
}

// jsonValue checks duplicate members at every nesting level instead of letting
// encoding/json silently overwrite an earlier value. A depth bound prevents
// small but deeply nested requests from exhausting the stack.
func jsonValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}
