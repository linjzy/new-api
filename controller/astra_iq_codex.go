package controller

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/gjson"
)

var astraIQCredentialPattern = regexp.MustCompile(`(?i)(?:bearer\s+\S+|sk-[a-z0-9_-]+|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret)["']?\s*[:=]\s*["']?[^\s,;"'}]+)`)

// Diagnostic text is visible with channel-read permission. Redact configured
// credentials before bounding it; never save raw bodies or CLI event envelopes.
func astraIQSafeErrorText(message string, ch *model.Channel, limit int) string {
	message, _, _ = strings.Cut(message, ", body:")
	message = sanitizeFetchModelsError(errors.New(message), ch.Key).Error()
	for _, value := range ch.GetHeaderOverride() {
		if secret, ok := value.(string); ok && secret != "" {
			message = sanitizeFetchModelsError(errors.New(message), secret).Error()
		}
	}
	message = astraIQCredentialPattern.ReplaceAllString(message, "[REDACTED]")
	message = common.MaskSensitiveInfo(message)
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return message
}

// The real pinned Codex CLI originates every IQ request. Its single-use local
// proxy selects exactly this channel and keeps provider keys out of the child
// environment, while reusing channel adapters, timing, usage and test logging.
func testAstraIQWithCodex(ctx context.Context, ch *model.Channel, userID int) testResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	binary, err := exec.LookPath("codex")
	if err != nil {
		return testResult{localErr: err, iqAttempt: model.AstraIQAttempt{Detail: "official_client_unavailable"}}
	}
	dir, err := os.MkdirTemp("", "astra-iq-codex-")
	if err != nil {
		return testResult{localErr: err}
	}
	defer os.RemoveAll(dir)
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		return testResult{localErr: err}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return testResult{localErr: err}
	}
	proxyKey := rand.Text()
	results := make(chan testResult, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: astraIQProxyHandler(ctx, ch, userID, proxyKey, results)}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	config := fmt.Sprintf(`model = "gpt-6-astra"
model_provider = "astra_iq"
model_reasoning_effort = "medium"
approval_policy = "never"
web_search = "disabled"
[features]
shell_tool = false
unified_exec = false
apps = false
plugins = false
multi_agent = false
browser_use = false
computer_use = false
js_repl = false
code_mode = false
code_mode_host = false
skill_search = false
view_image = false
sleep_tool = false
goals = false
[model_providers.astra_iq]
name = "Astra IQ"
base_url = "http://%s/v1"
wire_api = "responses"
env_key = "ASTRA_IQ_PROXY_KEY"
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 120000
supports_websockets = false
`, listener.Addr())
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		return testResult{localErr: err}
	}
	cmd := exec.CommandContext(ctx, binary, "exec", "--json", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "-C", workspace, "-")
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + dir, "ASTRA_IQ_PROXY_KEY=" + proxyKey}
	cmd.Stdin = strings.NewReader(astraIQQuestion)
	cmd.Stderr = io.Discard // CLI errors can contain request metadata; never persist them.
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return testResult{localErr: err}
	}
	if err := cmd.Start(); err != nil {
		return testResult{localErr: err, iqAttempt: model.AstraIQAttempt{Detail: "official_client_unavailable"}}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	completed, failed := false, false
	var answer, failureMessage string
	for scanner.Scan() {
		event := gjson.ParseBytes(scanner.Bytes())
		switch event.Get("type").String() {
		case "item.completed":
			if event.Get("item.type").String() == "agent_message" {
				answer = event.Get("item.text").String()
			}
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			failed = true
			failureMessage = event.Get("error.message").String()
			if failureMessage == "" {
				failureMessage = event.Get("message").String()
			}
		}
	}
	if scanner.Err() != nil {
		_ = cmd.Process.Kill()
	}
	cliErr := cmd.Wait()
	if failureMessage == "" && cliErr != nil {
		failureMessage = cliErr.Error()
	}
	failureMessage = strings.ReplaceAll(failureMessage, proxyKey, "[REDACTED]")
	var result testResult
	select {
	case result = <-results:
	default:
		return testResult{localErr: fmt.Errorf("official client did not issue a probe: %s", failureMessage), iqAttempt: model.AstraIQAttempt{Detail: "official_client_failed"}}
	}
	if result.localErr != nil || result.newAPIError != nil {
		if result.localErr != nil && strings.Contains(result.localErr.Error(), "only allows Codex official clients") {
			result.iqAttempt.Detail = "official_client_rejected"
		}
		return result
	}
	expected, gradeErr := astraIQAnswer(result.responseBody)
	if cliErr != nil || scanner.Err() != nil || failed || !completed || gradeErr != nil || strings.TrimSpace(answer) != expected {
		if failureMessage == "" && gradeErr != nil {
			failureMessage = gradeErr.Error()
		}
		if failureMessage == "" && scanner.Err() != nil {
			failureMessage = scanner.Err().Error()
		}
		if failureMessage == "" {
			failureMessage = "missing completion event or inconsistent final answer"
		}
		result.localErr = fmt.Errorf("official client did not complete the model response: %s", failureMessage)
		result.iqAttempt.Detail = "official_client_incomplete"
	}
	return result
}

// astraIQProxyHandler admits one authenticated Responses call for one channel.
func astraIQProxyHandler(ctx context.Context, ch *model.Channel, userID int, proxyKey string, results chan<- testResult) http.Handler {
	var used atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+proxyKey)) != 1 {
			http.Error(w, "invalid probe request", http.StatusForbidden)
			return
		}
		if !used.CompareAndSwap(false, true) {
			http.Error(w, "only one inference is allowed per check", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		result := testChannelWithClientRequest(ctx, ch, userID, model.AstraIQModel, string(constant.EndpointTypeOpenAIResponse), true, astraIQQuestion, r)
		results <- result
		if result.localErr != nil || result.newAPIError != nil {
			status := result.iqAttempt.HTTPStatus
			if status < 400 {
				status = http.StatusBadGateway
			}
			http.Error(w, "channel probe failed", status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(result.responseBody)
	})
}
