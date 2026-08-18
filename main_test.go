package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type mockBlock struct {
	data       []byte
	generation int64
}

type mockEntity struct {
	mu     sync.Mutex
	blocks map[string]mockBlock
	server *httptest.Server
}

func newMockEntity(t *testing.T) *mockEntity {
	t.Helper()
	mock := &mockEntity{blocks: map[string]mockBlock{}}
	mock.server = httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockEntity) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer test-key" {
		writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	key := request.URL.Path
	switch request.Method {
	case http.MethodGet:
		m.mu.Lock()
		block, exists := m.blocks[key]
		m.mu.Unlock()
		if !exists {
			writeJSON(response, http.StatusNotFound, map[string]string{"error": "block not found"})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{
			"data": base64.StdEncoding.EncodeToString(block.data),
			"metadata": map[string]string{
				"generation": fmt.Sprint(block.generation),
				"sizeBytes":  fmt.Sprint(len(block.data)),
				"sha256":     hashBytes(block.data),
			},
		})
	case http.MethodPut:
		var input map[string]string
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		data, err := base64.StdEncoding.DecodeString(input["data"])
		if err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		expected := numberField(input["ifGenerationMatch"])
		m.mu.Lock()
		block, exists := m.blocks[key]
		if (exists && expected != block.generation) || (!exists && expected != 0) {
			m.mu.Unlock()
			writeJSON(response, http.StatusConflict, map[string]string{"error": "generation mismatch"})
			return
		}
		block = mockBlock{data: append([]byte(nil), data...), generation: block.generation + 1}
		m.blocks[key] = block
		m.mu.Unlock()
		writeJSON(response, http.StatusOK, map[string]string{
			"generation": fmt.Sprint(block.generation),
			"sizeBytes":  fmt.Sprint(len(data)),
			"sha256":     hashBytes(data),
		})
	default:
		writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func testApplication(t *testing.T, mock *mockEntity) *application {
	t.Helper()
	metrics := &serviceMetrics{}
	entity, err := newEntityClient(mock.server.URL, "test-key", "entity-marketplace", "123", metrics)
	if err != nil {
		t.Fatal(err)
	}
	return newApplication(entity, metrics)
}

func TestMarketplaceCRUDAndRestartRecovery(t *testing.T) {
	mock := newMockEntity(t)
	app := testApplication(t, mock)
	if err := app.store.ensureSeed(t.Context()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.routes())

	listings := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/listings", nil, http.StatusOK)
	if got := len(listings["listings"].([]any)); got != 5 {
		t.Fatalf("expected 5 seed listings, got %d", got)
	}

	created := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/listings", map[string]any{
		"title": "Test compass", "description": "A durable brass compass for the integration test.",
		"category": "Outdoors", "priceCents": 4200, "seller": "Test Seller", "status": "available",
	}, http.StatusCreated)
	createdListing := created["listing"].(map[string]any)
	createdID := createdListing["id"].(string)
	server.Close()

	// A completely new application instance must recover the listing from the
	// Entity block without sharing in-memory state with the first instance.
	restarted := testApplication(t, mock)
	restartedServer := httptest.NewServer(restarted.routes())
	defer restartedServer.Close()
	recovered := requestJSON(t, restartedServer.Client(), http.MethodGet, restartedServer.URL+"/api/listings", nil, http.StatusOK)
	if got := len(recovered["listings"].([]any)); got != 6 {
		t.Fatalf("expected 6 listings after restart, got %d", got)
	}

	requestJSON(t, restartedServer.Client(), http.MethodDelete, restartedServer.URL+"/api/listings/"+createdID, nil, http.StatusOK)
	afterDelete := requestJSON(t, restartedServer.Client(), http.MethodGet, restartedServer.URL+"/api/listings", nil, http.StatusOK)
	if got := len(afterDelete["listings"].([]any)); got != 5 {
		t.Fatalf("expected 5 listings after delete, got %d", got)
	}
	proof := requestJSON(t, restartedServer.Client(), http.MethodGet, restartedServer.URL+"/_omnira/storage", nil, http.StatusOK)
	if proof["mode"] != "strict-entity-only" || proof["durableLocalDisk"] != false || proof["externalDatabase"] != false {
		t.Fatalf("unexpected strict storage proof: %#v", proof)
	}
}

func TestConcurrentMutationsUseCAS(t *testing.T) {
	mock := newMockEntity(t)
	app := testApplication(t, mock)
	if err := app.store.ensureSeed(t.Context()); err != nil {
		t.Fatal(err)
	}
	const writers = 6
	start := make(chan struct{})
	errorsByWriter := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			item := listing{ID: fmt.Sprintf("concurrent-%d", index), Title: fmt.Sprintf("Item %d", index), Description: "CAS concurrency test", Category: "Other", PriceCents: int64(index), Seller: "Test", Status: "available"}
			_, _, _, err := app.store.mutate(t.Context(), func(state *marketplaceState) (any, error) {
				state.Listings = append(state.Listings, item)
				return item, nil
			})
			errorsByWriter <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsByWriter)
	for err := range errorsByWriter {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, _, err := app.store.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(state.Listings); got != 5+writers {
		t.Fatalf("expected %d listings, got %d", 5+writers, got)
	}
	if app.store.metrics.snapshot().CASConflicts == 0 {
		t.Fatal("expected at least one CAS conflict during concurrent writes")
	}
}

func requestJSON(t *testing.T, client *http.Client, method, endpoint string, body any, expectedStatus int) map[string]any {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("expected HTTP %d, got %d: %s", expectedStatus, response.StatusCode, data)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
