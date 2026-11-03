package controller

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/drain"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomDrainResponsesCompletesAndSettlesBeforeClose(t *testing.T) {
	previous := drain.Default
	manager := drain.New()
	drain.Default = manager
	t.Cleanup(func() { drain.Default = previous })
	finish := make(chan struct{})
	var finishOnce sync.Once
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"draining","status":"in_progress"}}`))) {
			return
		}
		<-finish
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"draining","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
			return
		}
		_, _, err := ws.ReadMessage()
		assert.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "upstream receives service-restart close: %v", err)
	})
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"finish this request"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, manager.Begin("deploy-one"))
	assert.Equal(t, 1, manager.Status().WSBusy)
	assert.False(t, manager.Status().Ready)
	// A pre-existing socket cannot bypass deployment admission with another create.
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"do not start"}`)))
	rejection := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", rejection["type"])
	assert.EqualValues(t, http.StatusServiceUnavailable, rejection["status"])
	finishOnce.Do(func() { close(finish) })
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	assertResponsesWSAccounting(t, fixture, []int{1000})
	_, _, err := fixture.client.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "client receives terminal before restart close: %v", err)
	fixture.closeAndWait(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, manager.Wait(ctx))
	assert.True(t, manager.Status().Ready)
}

func TestCustomDrainIdleResponsesClosesWithoutAnUpstreamRequest(t *testing.T) {
	previous := drain.Default
	manager := drain.New()
	drain.Default = manager
	t.Cleanup(func() { drain.Default = previous })
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {
		t.Error("idle websocket must not contact upstream")
	})
	require.NoError(t, manager.Begin("deploy-idle"))
	require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, _, err := fixture.client.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseServiceRestart), "idle client receives restart close: %v", err)
	fixture.closeAndWait(t)
	assert.Zero(t, fixture.connections.Load())
	assert.True(t, manager.Status().Ready)
}

func TestCustomDrainHTTPWaitsForHandlerAndShutdownCannotBeCanceled(t *testing.T) {
	manager := drain.New()
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := manager.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			w.WriteHeader(http.StatusOK)
			return
		}
		close(started)
		<-finish
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	}()
	<-started
	manager.BeginShutdown()
	assert.Equal(t, 1, manager.Status().HTTPInflight)
	assert.False(t, manager.Status().Ready)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/v1/responses", http.StatusServiceUnavailable}, {"/api/status", http.StatusOK}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		assert.Equal(t, tc.status, response.Code)
	}
	require.Error(t, manager.Cancel("shutdown"))
	close(finish)
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.Wait(ctx))
}

func TestCustomDrainAdmissionAndCancelPreserveBusySession(t *testing.T) {
	for _, startFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "admission before drain", false: "drain before admission"}[startFirst], func(t *testing.T) {
			manager := drain.New()
			closed := make(chan struct{}, 1)
			var session *drain.Session
			manager.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				session = drain.RegisterWS(r.Context(), false, func() { closed <- struct{}{} })
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			require.NotNil(t, session)
			defer session.Unregister()
			if startFirst {
				require.True(t, session.StartWork())
			}
			require.NoError(t, manager.Begin("owner"))
			assert.False(t, session.StartWork(), "no task can enter after drain admission closes")
			require.Error(t, manager.Cancel("different-job"))
			if startFirst {
				assert.Empty(t, closed, "an accepted task must not be interrupted")
				require.NoError(t, manager.Cancel("owner"))
				session.FinishWork()
				assert.Empty(t, closed, "canceling maintenance keeps the busy session reusable")
				assert.True(t, session.StartWork())
				session.FinishWork()
			} else {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("idle session was not closed")
				}
			}
		})
	}
}

func TestCustomDrainControlRequiresUnixAccessAndMatchingJob(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "newapi-drain-test-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	path := filepath.Join(directory, "control.sock")
	manager := drain.New()
	stop, err := manager.StartControl(path)
	require.NoError(t, err)
	defer stop()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	_, err = manager.StartControl(path)
	require.ErrorContains(t, err, "already active")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, tc := range []struct {
		method, target string
		code           int
	}{
		{http.MethodGet, "/begin?job=owner", http.StatusMethodNotAllowed},
		{http.MethodPost, "/begin?job=bad%20job", http.StatusBadRequest},
		{http.MethodPost, "/begin?job=owner", http.StatusOK},
		{http.MethodPost, "/begin?job=other", http.StatusConflict},
		{http.MethodPost, "/cancel?job=other", http.StatusConflict},
		{http.MethodPost, "/cancel?job=owner", http.StatusOK},
		{http.MethodGet, "/status", http.StatusOK},
	} {
		request, err := http.NewRequest(tc.method, "http://unix"+tc.target, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		payload, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, tc.code, response.StatusCode, tc.target)
		if tc.target == "/status" {
			var status drain.Status
			require.NoError(t, common.Unmarshal(payload, &status))
			assert.False(t, status.Draining)
		}
	}
}
