// Package model contains the public storage API contracts. It has no dependency
// on the HTTP server, Kubernetes client, node Agent or persistence implementation.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const GiB int64 = 1024 * 1024 * 1024

type Object = map[string]any

type ListOptions struct {
	Limit, Offset      int
	Node, State, Query string
}

type Page struct {
	Items  []Object `json:"items"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

type Backend interface {
	Overview(context.Context) (Object, error)
	List(context.Context, string, ListOptions) (Page, error)
	Events(context.Context, ListOptions) (Page, error)
	Plan(context.Context, Object) (Object, error)
	CreatePool(context.Context, string, string) (Object, error)
	CreateVolume(context.Context, Object) (Object, error)
	Expand(context.Context, string, Object) (Object, error)
	Release(context.Context, string, string) (Object, error)
}

type Problem struct {
	Status int
	Detail string
}

func (p *Problem) Error() string                 { return p.Detail }
func NewProblem(status int, detail string) error { return &Problem{Status: status, Detail: detail} }

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.~-]{0,252}$`)

func Name(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !namePattern.MatchString(text) {
		return "", NewProblem(400, "名称须为 1–63 位小写字母、数字或连字符，首尾须为字母或数字")
	}
	return text, nil
}

func NodeName(value any) (string, error) {
	text, ok := value.(string)
	if !ok || len(text) > 253 {
		return "", NewProblem(400, "节点名须为有效的 DNS 子域名")
	}
	for _, label := range strings.Split(text, ".") {
		if !namePattern.MatchString(label) {
			return "", NewProblem(400, "节点名须为有效的 DNS 子域名")
		}
	}
	return text, nil
}

func Identifier(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !idPattern.MatchString(text) {
		return "", NewProblem(400, "资源 ID 必须为有效的字符串标识")
	}
	return text, nil
}

// SizeGiB requires an integer JSON token; strings, booleans, 1.0 and exponents
// are rejected consistently with the original Python API's integer contract.
func SizeGiB(value any) (int64, error) {
	var number int64
	var err error
	switch typed := value.(type) {
	case json.Number:
		number, err = typed.Int64()
	case int:
		number = int64(typed)
	case int64:
		number = typed
	case int32:
		number = int64(typed)
	default:
		err = fmt.Errorf("not an integer")
	}
	if err != nil || number < 1 || number > 1048576 {
		return 0, NewProblem(400, "容量必须为 1–1048576 的整数 GiB")
	}
	return number * GiB, nil
}

func String(value any) string { text, _ := value.(string); return text }

// Int64 reads already validated metadata, including persisted JSON numbers.
// User-entered capacities must go through SizeGiB instead.
func Int64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		number, _ := typed.Int64()
		return number
	case float64:
		if typed >= math.MinInt64 && typed < math.MaxInt64 && math.Trunc(typed) == typed {
			return int64(typed)
		}
	case string:
		number, _ := strconv.ParseInt(typed, 10, 64)
		return number
	}
	return 0
}

func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
