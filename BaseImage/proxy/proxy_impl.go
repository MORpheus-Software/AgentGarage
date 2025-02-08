package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker"
)

// Add these new environment variable getters at the top of the file
func getMarketplaceBaseURL() string {
    baseURL := os.Getenv("MARKETPLACE_URL")
    if baseURL == "" {
        baseURL = "http://marketplace:9000"
    }
    return baseURL
}

func getMarketplaceModelsEndpoint() string {
    return fmt.Sprintf("%s/blockchain/models", getMarketplaceBaseURL())
}

func getMarketplaceSessionEndpoint(modelID string) string {
    return fmt.Sprintf("%s/blockchain/models/%s/session", getMarketplaceBaseURL(), modelID)
}

func getMarketplaceChatEndpoint() string {
	marketplaceURL := os.Getenv("MARKETPLACE_URL")
	if marketplaceURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/chat/completions", marketplaceURL)
}

func getSessionExpirationSeconds() int {
	expirationStr := os.Getenv("SESSION_EXPIRATION_SECONDS")
	if expirationStr == "" {
		return 1800 // Default to 30 minutes
	}
	expiration, err := strconv.Atoi(expirationStr)
	if err != nil || expiration < 60 { // Minimum 1 minute
		log.Printf("Invalid SESSION_EXPIRATION_SECONDS value: %s, using default of 1800", expirationStr)
		return 1800
	}
	return expiration
}

// Update GetSessionID to include model context
func (sm *SessionManager) GetSessionInfo() (string, string) {
	return sm.SessionID, sm.ModelID
}

// Update UpdateSessionID to track model
func (sm *SessionManager) UpdateSession(sessionID, modelID string) {
	sm.SessionID = sessionID
	sm.ModelID = modelID
}

// SessionManagerInstance is a global instance of SessionManager
var SessionManagerInstance = &SessionManager{}

// Add these new vars at the top of the file
var (
	defaultTimeout = 30 * time.Second
	circuitBreaker *gobreaker.CircuitBreaker
	sessionExpirationSeconds = getSessionExpirationSeconds()

	// Session and model caches with mutex protection
	sessionCache = struct {
		sync.RWMutex
		m map[string]CachedSession
	}{m: make(map[string]CachedSession)}

	modelCache = struct {
		sync.RWMutex
		m map[string]CachedModel
	}{m: make(map[string]CachedModel)}

	consumerNodeURL = getEnvOrDefault("CONSUMER_NODE_URL", "https://consumer-node-yalzemm5uq-uc.a.run.app")

	// Add a flag to control cleanup goroutine
	enableCleanupGoroutine = true
)

func init() {
	// Configure circuit breaker
	circuitBreaker = gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "marketplace",
		MaxRequests: 3,
		Interval:    10 * time.Second,
		Timeout:     60 * time.Second,
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Printf("Circuit breaker state changed from %v to %v", from, to)
		},
	})

	// Add periodic cleanup of expired sessions only if enabled
	if enableCleanupGoroutine {
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			for range ticker.C {
				cleanupExpiredSessions()
			}
		}()
	}
}

// Update activeSessions to manage sessions per model ID
var (
	activeSessions = make(map[string]*MorpheusSession)
	sessionMutex   sync.Mutex
)

// Add retry configuration constants
const (
	maxRetries = 3
	baseDelay  = 1 * time.Second
)

// calculateSimilarity returns a similarity score between two strings
func calculateSimilarity(s1, s2 string) float64 {
	// Convert to lowercase for case-insensitive comparison
	s1 = strings.ToLower(s1)
	s2 = strings.ToLower(s2)

	// If either string is empty
	if len(s1) == 0 || len(s2) == 0 {
		if len(s1) == len(s2) {
			return 1.0 // both empty
		}
		return 0.0 // one is empty
	}

	// If strings are identical
	if s1 == s2 {
		return 1.0
	}

	// Calculate Levenshtein distance
	distance := levenshteinDistance(s1, s2)
	maxLen := float64(max(len(s1), len(s2)))

	// Calculate similarity score and round to nearest 0.1
	similarity := 1.0 - float64(distance)/maxLen
	return float64(int(similarity*10+0.5)) / 10.0
}

// Helper function to find maximum of two integers
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Fix levenshteinDistance function
func levenshteinDistance(s1, s2 string) int {
	if len(s1) == 0 {
		return len(s2)
	}
	if len(s2) == 0 {
		return len(s1)
	}

	// Convert strings to runes for proper UTF-8 handling
	r1 := []rune(strings.ToLower(s1))
	r2 := []rune(strings.ToLower(s2))

	// Create matrix
	rows := len(r1) + 1
	cols := len(r2) + 1
	matrix := make([][]int, rows)
	for i := range matrix {
		matrix[i] = make([]int, cols)
		matrix[i][0] = i // Initialize first column
	}

	// Initialize first row
	for j := 0; j < cols; j++ {
		matrix[0][j] = j
	}

	// Fill rest of the matrix
	for i := 1; i < rows; i++ {
		for j := 1; j < cols; j++ {
			cost := 1
			if r1[i-1] == r2[j-1] {
				cost = 0
			}
			matrix[i][j] = min3(
				matrix[i-1][j]+1,      // deletion
				matrix[i][j-1]+1,      // insertion
				matrix[i-1][j-1]+cost, // substitution
			)
		}
	}

	return matrix[rows-1][cols-1]
}

// Add helper function for min of 3 numbers
func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// Helper function to get environment variable with default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// Add cleanup function for expired sessions
func cleanupExpiredSessions() {
	sessionMutex.Lock()
	defer sessionMutex.Unlock()

	for modelID, session := range activeSessions {
		if time.Since(session.Created) > time.Duration(sessionExpirationSeconds)*time.Second {
			delete(activeSessions, modelID)
			log.Printf("Cleaned up expired session for model %s", modelID)
		}
	}
}

// ensureSession ensures there is an active session for the given model ID
func ensureSession(modelID string) error {
	sessionMutex.Lock()
	defer sessionMutex.Unlock()

	// Check if we have an active session
	if session, exists := activeSessions[modelID]; exists {
		if time.Since(session.Created) < time.Duration(sessionExpirationSeconds)*time.Second {
			return nil // Session is still valid
		}
		delete(activeSessions, modelID) // Remove expired session
	}

	// Create new session
	endpoint := getMarketplaceSessionEndpoint(modelID)
	resp, err := http.Post(endpoint, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create session, status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode session response: %v", err)
	}

	// Store new session
	activeSessions[modelID] = &MorpheusSession{
		SessionID: result.SessionID,
		ModelID:   modelID,
		Created:   time.Now(),
	}

	return nil
}

// validateModelHandle checks if the model handle is valid and returns the corresponding ID
func validateModelHandle(handle string) (string, error) {
	modelID, err := findModelID(handle)
	if err != nil {
		if err.Error() == "No Supported Model Has Been Registered" {
			return "", err
		}
		// For any other error, return the standard message
		return "", fmt.Errorf("No Supported Model Has Been Registered")
	}
	return modelID, nil
}

// Update findModelID to be more robust
func findModelID(modelHandle string) (string, error) {
	// Add debug logging
	log.Printf("Attempting to find model ID for handle: '%s'", modelHandle)

	// Normalize input
	modelHandle = strings.TrimSpace(modelHandle)
	if modelHandle == "" {
		return "", fmt.Errorf("model handle cannot be empty")
	}

	// Check cache first
	modelCache.RLock()
	if cached, exists := modelCache.m[modelHandle]; exists && time.Since(cached.Created) < time.Hour {
		modelCache.RUnlock()
		log.Printf("Found cached model ID for '%s': %s", modelHandle, cached.ModelID)
		return cached.ModelID, nil
	}
	modelCache.RUnlock()

	endpoint := getMarketplaceModelsEndpoint()
	log.Printf("Fetching models from: %s", endpoint)

	// Query the marketplace API
	resp, err := http.Get(endpoint)
	if err != nil {
		return "", fmt.Errorf("failed to fetch models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to fetch models, status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Models []ModelInfo `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode models response: %v", err)
	}

	// Find best match
	var bestMatch struct {
		modelID    string
		similarity float64
	}

	for _, model := range result.Models {
		similarity := calculateSimilarity(modelHandle, model.Name)
		if similarity > bestMatch.similarity {
			bestMatch.modelID = model.Id
			bestMatch.similarity = similarity
		}
	}

	if bestMatch.similarity >= 0.8 {
		log.Printf("Found model ID for '%s': %s (similarity: %.2f)", modelHandle, bestMatch.modelID, bestMatch.similarity)
		return bestMatch.modelID, nil
	}

	return "", fmt.Errorf("no matching model found for '%s'", modelHandle)
}

// getModels fetches the list of available models from the consumer node
func getModels() ([]Model, error) {
	modelsURL := fmt.Sprintf("%s/blockchain/models", consumerNodeURL)
	resp, err := http.Get(modelsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to fetch models: %s", string(bodyBytes))
	}

	var result struct {
		Models []Model `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode models response: %v", err)
	}

	return result.Models, nil
}

// setStreamingHeaders sets the necessary headers for streaming responses
func setStreamingHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// copyHeaders copies headers from the marketplace response to the client response
func copyHeaders(w http.ResponseWriter, headers http.Header) {
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

// respondWithError sends an error response to the client
func respondWithError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// NewProxy creates a new instance of Proxy
func NewProxy() *Proxy {
	return &Proxy{
		client: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// Add the getMarketplaceBaseURL method to Proxy
func (p *Proxy) getMarketplaceBaseURL() string {
	if url := os.Getenv("MARKETPLACE_URL"); url != "" {
		return url
	}
	return "https://consumer-node-yalzemm5uq-uc.a.run.app"
}

// Add the createSession method to Proxy
func (p *Proxy) createSession(modelID string) (string, error) {
	endpoint := getMarketplaceSessionEndpoint(modelID)
	resp, err := p.client.Post(endpoint, "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to create session, status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode session response: %v", err)
	}

	// Cache the session
	sessionCache.Lock()
	sessionCache.m[result.SessionID] = CachedSession{
		SessionID:  result.SessionID,
		ModelID:    modelID,
		ExpiresAt:  time.Now().Add(time.Duration(sessionExpirationSeconds) * time.Second),
	}
	sessionCache.Unlock()

	return result.SessionID, nil
} 