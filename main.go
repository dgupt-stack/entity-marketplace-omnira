package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultEntityURL       = "https://entityservice-k4u67azzg5.app.omnira.dev"
	defaultOwnerID         = "5695892345266999354"
	defaultNamespace       = "entity-marketplace"
	defaultCredentialPath  = "/Users/djgupt/api-keys/paperclip-omnira-entity-key.txt"
	stateKey               = "marketplace/state.json"
	maxStateBytes          = 512 * 1024
	maxListings            = 250
	maxMutationAttempts    = 8
	entityRequestTimeout   = 20 * time.Second
	entityRetryBase        = 125 * time.Millisecond
	entityRetryMaximum     = 2 * time.Second
	publicMarketplaceTitle = "GoodMarket"
)

//go:embed static/*
var staticFiles embed.FS

type listing struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceCents  int64  `json:"priceCents"`
	Seller      string `json:"seller"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type marketplaceState struct {
	SchemaVersion int       `json:"schemaVersion"`
	Revision      int64     `json:"revision"`
	UpdatedAt     string    `json:"updatedAt"`
	Listings      []listing `json:"listings"`
}

type entityMetadata struct {
	Generation int64
	SizeBytes  int64
	SHA256     string
	UpdatedAt  string
}

type entityError struct {
	Status int
	Body   string
}

func (e *entityError) Error() string {
	return fmt.Sprintf("Entity Service returned HTTP %d", e.Status)
}

func (e *entityError) conflict() bool { return e.Status == http.StatusConflict }
func (e *entityError) notFound() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

type serviceMetrics struct {
	mu                sync.Mutex
	entityReads       int64
	entityWrites      int64
	entityRetries     int64
	casConflicts      int64
	lastReadDuration  time.Duration
	lastWriteDuration time.Duration
	lastError         string
}

type metricsSnapshot struct {
	EntityReads     int64   `json:"entityReads"`
	EntityWrites    int64   `json:"entityWrites"`
	EntityRetries   int64   `json:"entityRetries"`
	CASConflicts    int64   `json:"casConflicts"`
	LastReadMs      float64 `json:"lastReadMs"`
	LastWriteMs     float64 `json:"lastWriteMs"`
	LastEntityError string  `json:"lastEntityError,omitempty"`
}

func (m *serviceMetrics) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return metricsSnapshot{
		EntityReads:     m.entityReads,
		EntityWrites:    m.entityWrites,
		EntityRetries:   m.entityRetries,
		CASConflicts:    m.casConflicts,
		LastReadMs:      durationMilliseconds(m.lastReadDuration),
		LastWriteMs:     durationMilliseconds(m.lastWriteDuration),
		LastEntityError: m.lastError,
	}
}

func durationMilliseconds(value time.Duration) float64 {
	return float64(value.Microseconds()) / 1000
}

type entityClient struct {
	baseURL     string
	apiKey      string
	namespace   string
	ownerID     string
	httpClient  *http.Client
	metrics     *serviceMetrics
	maxAttempts int
}

func newEntityClient(baseURL, apiKey, namespace, ownerID string, metrics *serviceMetrics) (*entityClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(namespace) == "" {
		return nil, errors.New("Entity Service URL, API key, and namespace are required")
	}
	if _, err := strconv.ParseUint(ownerID, 10, 64); err != nil {
		return nil, errors.New("Entity Service owner ID must be a positive integer")
	}
	return &entityClient{
		baseURL:     baseURL,
		apiKey:      strings.TrimSpace(apiKey),
		namespace:   strings.TrimSpace(namespace),
		ownerID:     ownerID,
		httpClient:  &http.Client{Timeout: entityRequestTimeout},
		metrics:     metrics,
		maxAttempts: 6,
	}, nil
}

func (c *entityClient) blockURL(key string) string {
	parts := strings.Split(key, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return c.baseURL + "/v1/blocks/" + url.PathEscape(c.namespace) + "/" + strings.Join(parts, "/")
}

func (c *entityClient) request(ctx context.Context, method, key string, input any) ([]byte, error) {
	var encoded []byte
	var err error
	if input != nil {
		encoded, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}

	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		var body io.Reader
		if encoded != nil {
			body = strings.NewReader(string(encoded))
		}
		request, requestErr := http.NewRequestWithContext(ctx, method, c.blockURL(key), body)
		if requestErr != nil {
			return nil, requestErr
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
		if encoded != nil {
			request.Header.Set("Content-Type", "application/json")
		}

		response, requestErr := c.httpClient.Do(request)
		if requestErr != nil {
			c.recordError(requestErr)
			return nil, fmt.Errorf("Entity Service request failed: %w", requestErr)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
		_ = response.Body.Close()
		if readErr != nil {
			c.recordError(readErr)
			return nil, fmt.Errorf("read Entity Service response: %w", readErr)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			c.clearError()
			return responseBody, nil
		}
		if transientStatus(response.StatusCode) && attempt < c.maxAttempts {
			c.metrics.mu.Lock()
			c.metrics.entityRetries++
			c.metrics.mu.Unlock()
			delay := entityRetryBase << (attempt - 1)
			if delay > entityRetryMaximum {
				delay = entityRetryMaximum
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		serviceErr := &entityError{Status: response.StatusCode, Body: string(responseBody)}
		c.recordError(serviceErr)
		return nil, serviceErr
	}
	return nil, errors.New("Entity Service retry limit reached")
}

func transientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func (c *entityClient) recordError(err error) {
	c.metrics.mu.Lock()
	defer c.metrics.mu.Unlock()
	c.metrics.lastError = err.Error()
}

func (c *entityClient) clearError() {
	c.metrics.mu.Lock()
	defer c.metrics.mu.Unlock()
	c.metrics.lastError = ""
}

func (c *entityClient) get(ctx context.Context, key string) ([]byte, entityMetadata, error) {
	started := time.Now()
	body, err := c.request(ctx, http.MethodGet, key, nil)
	c.metrics.mu.Lock()
	c.metrics.entityReads++
	c.metrics.lastReadDuration = time.Since(started)
	c.metrics.mu.Unlock()
	if err != nil {
		return nil, entityMetadata{}, err
	}
	var envelope struct {
		Data     string         `json:"data"`
		Metadata map[string]any `json:"metadata"`
		Block    map[string]any `json:"block"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, entityMetadata{}, fmt.Errorf("decode Entity block: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return nil, entityMetadata{}, fmt.Errorf("decode Entity block data: %w", err)
	}
	metadataMap := envelope.Metadata
	if len(metadataMap) == 0 {
		metadataMap = envelope.Block
	}
	return data, parseMetadata(metadataMap), nil
}

func (c *entityClient) put(ctx context.Context, key string, data []byte, generation int64) (entityMetadata, error) {
	started := time.Now()
	input := map[string]string{
		"data":              base64.StdEncoding.EncodeToString(data),
		"contentType":       "application/json",
		"ownerEntityId":     c.ownerID,
		"ifGenerationMatch": strconv.FormatInt(generation, 10),
	}
	body, err := c.request(ctx, http.MethodPut, key, input)
	c.metrics.mu.Lock()
	c.metrics.entityWrites++
	c.metrics.lastWriteDuration = time.Since(started)
	c.metrics.mu.Unlock()
	if err != nil {
		return entityMetadata{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return entityMetadata{}, fmt.Errorf("decode Entity metadata: %w", err)
	}
	metadata := parseMetadata(raw)
	if metadata.SizeBytes == 0 {
		metadata.SizeBytes = int64(len(data))
	}
	return metadata, nil
}

func parseMetadata(raw map[string]any) entityMetadata {
	return entityMetadata{
		Generation: numberField(raw["generation"]),
		SizeBytes:  numberField(raw["sizeBytes"]),
		SHA256:     stringField(raw["sha256"]),
		UpdatedAt:  stringField(raw["updatedAt"]),
	}
}

func numberField(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	case string:
		result, _ := strconv.ParseInt(typed, 10, 64)
		return result
	default:
		return 0
	}
}

func stringField(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

type marketplaceStore struct {
	entity  *entityClient
	metrics *serviceMetrics
}

func (s *marketplaceStore) load(ctx context.Context) (marketplaceState, entityMetadata, error) {
	data, metadata, err := s.entity.get(ctx, stateKey)
	if err != nil {
		var serviceErr *entityError
		if errors.As(err, &serviceErr) && serviceErr.notFound() {
			return marketplaceState{SchemaVersion: 1, Listings: []listing{}}, entityMetadata{}, nil
		}
		return marketplaceState{}, entityMetadata{}, err
	}
	var state marketplaceState
	if err := json.Unmarshal(data, &state); err != nil {
		return marketplaceState{}, entityMetadata{}, fmt.Errorf("decode marketplace state: %w", err)
	}
	if state.Listings == nil {
		state.Listings = []listing{}
	}
	return state, metadata, nil
}

func (s *marketplaceStore) save(ctx context.Context, state marketplaceState, generation int64) (entityMetadata, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return entityMetadata{}, err
	}
	if len(data) > maxStateBytes {
		return entityMetadata{}, fmt.Errorf("marketplace state is %d bytes; the safe single-block limit is %d", len(data), maxStateBytes)
	}
	return s.entity.put(ctx, stateKey, data, generation)
}

func (s *marketplaceStore) ensureSeed(ctx context.Context) error {
	state, metadata, err := s.load(ctx)
	if err != nil {
		return err
	}
	if metadata.Generation > 0 || len(state.Listings) > 0 {
		return nil
	}
	state = seedState()
	_, err = s.save(ctx, state, 0)
	if err != nil {
		var serviceErr *entityError
		if errors.As(err, &serviceErr) && serviceErr.conflict() {
			return nil
		}
	}
	return err
}

func (s *marketplaceStore) mutate(ctx context.Context, change func(*marketplaceState) (any, error)) (any, marketplaceState, entityMetadata, error) {
	for attempt := 1; attempt <= maxMutationAttempts; attempt++ {
		state, metadata, err := s.load(ctx)
		if err != nil {
			return nil, marketplaceState{}, entityMetadata{}, err
		}
		result, err := change(&state)
		if err != nil {
			return nil, marketplaceState{}, entityMetadata{}, err
		}
		state.Revision++
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		saved, err := s.save(ctx, state, metadata.Generation)
		if err == nil {
			return result, state, saved, nil
		}
		var serviceErr *entityError
		if errors.As(err, &serviceErr) && serviceErr.conflict() {
			s.metrics.mu.Lock()
			s.metrics.casConflicts++
			s.metrics.mu.Unlock()
			continue
		}
		return nil, marketplaceState{}, entityMetadata{}, err
	}
	return nil, marketplaceState{}, entityMetadata{}, errors.New("marketplace update exceeded the CAS retry limit")
}

type application struct {
	store     *marketplaceStore
	startedAt time.Time
}

func newApplication(entity *entityClient, metrics *serviceMetrics) *application {
	return &application{store: &marketplaceStore{entity: entity, metrics: metrics}, startedAt: time.Now().UTC()}
}

func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.serveIndex)
	mux.HandleFunc("GET /app.js", app.serveStatic("static/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /styles.css", app.serveStatic("static/styles.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /api/listings", app.listListings)
	mux.HandleFunc("POST /api/listings", app.createListing)
	mux.HandleFunc("PATCH /api/listings/{id}", app.updateListing)
	mux.HandleFunc("DELETE /api/listings/{id}", app.deleteListing)
	mux.HandleFunc("GET /api/health", app.health)
	mux.HandleFunc("GET /api/diagnostics", app.diagnostics)
	mux.HandleFunc("GET /_omnira/storage", app.storageProof)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		response.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(response, request)
	})
}

func (app *application) serveIndex(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(response, request)
		return
	}
	app.serveStatic("static/index.html", "text/html; charset=utf-8")(response, request)
}

func (app *application) serveStatic(name, contentType string) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		data, err := staticFiles.ReadFile(name)
		if err != nil {
			http.Error(response, "asset unavailable", http.StatusInternalServerError)
			return
		}
		response.Header().Set("Content-Type", contentType)
		response.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = response.Write(data)
	}
}

func (app *application) listListings(response http.ResponseWriter, request *http.Request) {
	state, metadata, err := app.store.load(request.Context())
	if err != nil {
		writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"listings": state.Listings,
		"revision": state.Revision,
		"storage":  map[string]any{"generation": metadata.Generation, "sizeBytes": metadata.SizeBytes},
	})
}

type listingInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceCents  int64  `json:"priceCents"`
	Seller      string `json:"seller"`
	Status      string `json:"status"`
}

func decodeListingInput(request *http.Request) (listingInput, error) {
	var input listingInput
	decoder := json.NewDecoder(io.LimitReader(request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, errors.New("invalid listing JSON")
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.Category = strings.TrimSpace(input.Category)
	input.Seller = strings.TrimSpace(input.Seller)
	input.Status = strings.TrimSpace(input.Status)
	if input.Status == "" {
		input.Status = "available"
	}
	if input.Title == "" || len(input.Title) > 80 {
		return input, errors.New("title is required and must be at most 80 characters")
	}
	if input.Description == "" || len(input.Description) > 500 {
		return input, errors.New("description is required and must be at most 500 characters")
	}
	if input.Seller == "" || len(input.Seller) > 60 {
		return input, errors.New("seller is required and must be at most 60 characters")
	}
	if input.Category == "" || len(input.Category) > 40 {
		return input, errors.New("category is required and must be at most 40 characters")
	}
	if input.PriceCents < 0 || input.PriceCents > 100_000_000 {
		return input, errors.New("price is outside the supported range")
	}
	if input.Status != "available" && input.Status != "reserved" && input.Status != "sold" {
		return input, errors.New("status must be available, reserved, or sold")
	}
	return input, nil
}

func (app *application) createListing(response http.ResponseWriter, request *http.Request) {
	input, err := decodeListingInput(request)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	created := listing{
		ID: randomID(), Title: input.Title, Description: input.Description,
		Category: input.Category, PriceCents: input.PriceCents, Seller: input.Seller,
		Status: input.Status, CreatedAt: now, UpdatedAt: now,
	}
	result, state, metadata, err := app.store.mutate(request.Context(), func(state *marketplaceState) (any, error) {
		if len(state.Listings) >= maxListings {
			return nil, fmt.Errorf("marketplace reached its %d-listing experiment limit", maxListings)
		}
		state.Listings = append([]listing{created}, state.Listings...)
		return created, nil
	})
	if err != nil {
		writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"listing": result, "revision": state.Revision, "generation": metadata.Generation})
}

func (app *application) updateListing(response http.ResponseWriter, request *http.Request) {
	input, err := decodeListingInput(request)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id := request.PathValue("id")
	result, state, metadata, err := app.store.mutate(request.Context(), func(state *marketplaceState) (any, error) {
		for index := range state.Listings {
			if state.Listings[index].ID != id {
				continue
			}
			state.Listings[index].Title = input.Title
			state.Listings[index].Description = input.Description
			state.Listings[index].Category = input.Category
			state.Listings[index].PriceCents = input.PriceCents
			state.Listings[index].Seller = input.Seller
			state.Listings[index].Status = input.Status
			state.Listings[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return state.Listings[index], nil
		}
		return nil, errListingNotFound
	})
	if err != nil {
		writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"listing": result, "revision": state.Revision, "generation": metadata.Generation})
}

var errListingNotFound = errors.New("listing not found")

func (app *application) deleteListing(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	_, state, metadata, err := app.store.mutate(request.Context(), func(state *marketplaceState) (any, error) {
		for index := range state.Listings {
			if state.Listings[index].ID != id {
				continue
			}
			state.Listings = append(state.Listings[:index], state.Listings[index+1:]...)
			return nil, nil
		}
		return nil, errListingNotFound
	})
	if err != nil {
		writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"deleted": true, "revision": state.Revision, "generation": metadata.Generation})
}

func (app *application) health(response http.ResponseWriter, request *http.Request) {
	state, metadata, err := app.store.load(request.Context())
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"ok": true, "service": "entity-marketplace", "mode": "strict-entity-only",
		"listings": len(state.Listings), "revision": state.Revision, "generation": metadata.Generation,
	})
}

func (app *application) diagnostics(response http.ResponseWriter, request *http.Request) {
	state, metadata, err := app.store.load(request.Context())
	if err != nil {
		writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"metrics": app.store.metrics.snapshot(),
		"state": map[string]any{
			"listings": len(state.Listings), "revision": state.Revision,
			"generation": metadata.Generation, "sizeBytes": metadata.SizeBytes,
			"safeLimitBytes": maxStateBytes, "utilizationPercent": float64(metadata.SizeBytes) / maxStateBytes * 100,
		},
		"knownLimitations": []string{
			"Every mutation rewrites one JSON block.",
			"Concurrent writes serialize through generation-based CAS retries.",
			"The baseline caps state at 512 KiB and 250 listings.",
			"Entity Service provides blocks, not relational queries or secondary indexes.",
		},
	})
}

func (app *application) storageProof(response http.ResponseWriter, request *http.Request) {
	state, metadata, err := app.store.load(request.Context())
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "mode": "strict-entity-only", "phase": "entity-unavailable", "error": err.Error(),
		})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"ok":                  true,
		"mode":                "strict-entity-only",
		"phase":               "ready",
		"durableStore":        "Omnira Entity Service BlockStore",
		"durableLocalDisk":    false,
		"externalDatabase":    false,
		"namespace":           app.store.entity.namespace,
		"stateKey":            stateKey,
		"stateGeneration":     metadata.Generation,
		"stateBytes":          metadata.SizeBytes,
		"stateSHA256":         metadata.SHA256,
		"stateUpdatedAt":      metadata.UpdatedAt,
		"marketplaceRevision": state.Revision,
		"listingCount":        len(state.Listings),
		"processStartedAt":    app.startedAt.Format(time.RFC3339Nano),
		"metrics":             app.store.metrics.snapshot(),
		"limits":              map[string]any{"safeStateBytes": maxStateBytes, "maxListings": maxListings, "casAttempts": maxMutationAttempts},
	})
}

func writeAPIError(response http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, errListingNotFound) {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "experiment limit") || strings.Contains(err.Error(), "safe single-block limit") {
		status = http.StatusRequestEntityTooLarge
	}
	writeJSON(response, status, map[string]any{"error": err.Error()})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func randomID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(bytes[:])
}

func seedState() marketplaceState {
	created := "2026-08-17T19:00:00Z"
	return marketplaceState{
		SchemaVersion: 1,
		Revision:      1,
		UpdatedAt:     created,
		Listings: []listing{
			{ID: "seed-field-camera", Title: "Field Camera", Description: "Weather-sealed mirrorless camera with a compact prime lens. Ready for the next trail.", Category: "Cameras", PriceCents: 68000, Seller: "Maya R.", Status: "available", CreatedAt: created, UpdatedAt: created},
			{ID: "seed-oak-stool", Title: "Oak Studio Stool", Description: "Hand-finished white oak stool made by a neighborhood woodworker. One of a tiny batch.", Category: "Home", PriceCents: 14500, Seller: "North & Grain", Status: "available", CreatedAt: created, UpdatedAt: created},
			{ID: "seed-synth", Title: "Pocket Synth", Description: "A playful battery-powered synthesizer with sequencer, case, and patch guide.", Category: "Music", PriceCents: 21900, Seller: "Jon Bell", Status: "reserved", CreatedAt: created, UpdatedAt: created},
			{ID: "seed-cruiser", Title: "City Cruiser", Description: "Recently serviced steel-frame bicycle with new tires, lights, and a front rack.", Category: "Outdoors", PriceCents: 39000, Seller: "Ari C.", Status: "available", CreatedAt: created, UpdatedAt: created},
			{ID: "seed-ceramics", Title: "Morning Ceramics", Description: "Two wheel-thrown stoneware mugs with a speckled glaze. Dishwasher safe.", Category: "Home", PriceCents: 5800, Seller: "Soft Earth", Status: "sold", CreatedAt: created, UpdatedAt: created},
		},
	}
}

func credentialFromEnvironment() (string, error) {
	if value := strings.TrimSpace(os.Getenv("OMNIRA_ENTITY_API_KEY")); value != "" {
		return value, nil
	}
	path := strings.TrimSpace(os.Getenv("ENTITY_MARKET_CREDENTIAL_FILE"))
	if path == "" {
		path = defaultCredentialPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Entity credential unavailable: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("Entity credential is empty")
	}
	return value, nil
}

func serviceAddress(args []string) string {
	host := "0.0.0.0"
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "3100"
	}
	for index := 0; index < len(args); index++ {
		switch argument := args[index]; {
		case strings.HasPrefix(argument, "--port="):
			port = strings.TrimPrefix(argument, "--port=")
		case argument == "--port" && index+1 < len(args):
			index++
			port = args[index]
		case strings.HasPrefix(argument, "--host="):
			host = strings.TrimPrefix(argument, "--host=")
		case argument == "--host" && index+1 < len(args):
			index++
			host = args[index]
		}
	}
	return host + ":" + port
}

func main() {
	credential, err := credentialFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	metrics := &serviceMetrics{}
	entity, err := newEntityClient(
		valueOrDefault("OMNIRA_ENTITY_URL", defaultEntityURL),
		credential,
		valueOrDefault("OMNIRA_ENTITY_NAMESPACE", defaultNamespace),
		valueOrDefault("OMNIRA_ENTITY_OWNER_ID", defaultOwnerID),
		metrics,
	)
	if err != nil {
		log.Fatal(err)
	}
	app := newApplication(entity, metrics)
	startupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := app.store.ensureSeed(startupContext); err != nil {
		log.Fatalf("initialize Entity marketplace: %v", err)
	}
	address := serviceAddress(os.Args[1:])
	log.Printf("%s listening on %s (strict Entity-only, namespace %s)", publicMarketplaceTitle, address, entity.namespace)
	server := &http.Server{Addr: address, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
