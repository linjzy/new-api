package drain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const SocketPath = "/tmp/newapi-drain/control.sock"

var validJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

// ControlHandler is served only on a mode-0600 Unix socket, never on the public
// router. Docker exec grants the host operator access without an API credential.
func (m *Manager) ControlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		status := http.StatusOK
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
		case r.Method == http.MethodPost && (r.URL.Path == "/begin" || r.URL.Path == "/cancel"):
			job := r.URL.Query().Get("job")
			if !validJobID.MatchString(job) || job == "shutdown" {
				http.Error(w, "invalid drain job", http.StatusBadRequest)
				return
			}
			var err error
			if r.URL.Path == "/begin" {
				err = m.Begin(job)
			} else {
				err = m.Cancel(job)
			}
			if err != nil {
				status = http.StatusConflict
			}
		default:
			http.Error(w, "unsupported drain operation", http.StatusMethodNotAllowed)
			return
		}
		payload, err := common.Marshal(m.Status())
		if err != nil {
			http.Error(w, "cannot encode drain status", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	})
}

func (m *Manager) StartControl(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("drain control directory must be private")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("drain control path is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("drain control socket is already active")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{Handler: m.ControlHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}

func RunCLI(args []string) int {
	if len(args) == 0 || (args[0] != "status" && args[0] != "begin" && args[0] != "cancel") ||
		(args[0] == "status" && len(args) != 1) || (args[0] != "status" && (len(args) != 2 || !validJobID.MatchString(args[1]))) {
		fmt.Fprintln(os.Stderr, "usage: new-api drain status | begin JOB_ID | cancel JOB_ID")
		return 2
	}
	method, target := http.MethodGet, "http://unix/status"
	if args[0] != "status" {
		method, target = http.MethodPost, "http://unix/"+args[0]+"?job="+url.QueryEscape(args[1])
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, _ := http.NewRequest(method, target, nil)
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drain control unavailable:", err)
		return 1
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	if err != nil || response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "drain control rejected operation:", response.Status)
		return 1
	}
	var status Status
	if common.Unmarshal(payload, &status) != nil || status.Protocol != 1 {
		fmt.Fprintln(os.Stderr, "invalid drain control response")
		return 1
	}
	fmt.Println(string(payload))
	return 0
}
