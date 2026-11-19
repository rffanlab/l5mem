// Package client：命令行和其他程序调 L5 服务用。优先走本机 Unix socket，没有就走 HTTP。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	base   string
	hc     *http.Client
	token  string
	Caller string
	Via    string
}

const DefaultSocket = "/run/l5mem/l5.sock"

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func New() *Client {
	caller := env("L5_CALLER", "")
	if caller == "" {
		h, _ := os.Hostname()
		caller = env("USER", "cli") + "@" + h
	}
	c := &Client{token: os.Getenv("L5_TOKEN"), Caller: caller}
	sock := env("L5_SOCKET", DefaultSocket)
	if u := os.Getenv("L5_URL"); u == "" {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			c.base = "http://l5"
			c.Via = "socket " + sock
			c.hc = &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				}}}
			return c
		}
	}
	c.base = strings.TrimRight(env("L5_URL", "http://127.0.0.1:19110"), "/")
	c.Via = c.base
	c.hc = &http.Client{Timeout: 10 * time.Minute}
	return c
}

type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return e.Msg }

// Call 调一个接口。body 为 nil 时用 GET + query。
func (c *Client) Call(op string, query url.Values, body any, out any) error {
	u := c.base + "/api/v1/" + op
	if query == nil {
		query = url.Values{}
	}
	query.Set("caller", c.Caller)
	u += "?" + query.Encode()
	var req *http.Request
	var err error
	if body != nil {
		var rd io.Reader
		switch b := body.(type) {
		case io.Reader:
			rd = b
		default:
			buf, _ := json.Marshal(body)
			rd = bytes.NewReader(buf)
		}
		req, err = http.NewRequest(http.MethodPost, u, rd)
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequest(http.MethodGet, u, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("X-L5-Caller", c.Caller)
	if c.token != "" {
		req.Header.Set("X-L5-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 L5 服务（%s）：%w", c.Via, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &APIError{resp.StatusCode, e.Error}
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
