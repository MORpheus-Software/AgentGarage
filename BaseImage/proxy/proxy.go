package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker"
)

// SessionManager manages session states
type SessionManager struct {
    SessionID string
    // Add more fields if necessary
}

// GetSessionID retrieves the current session ID
func (sm *SessionManager) GetSessionID() string {
    return sm.SessionID
}

// UpdateSessionID updates the session ID
func (sm *SessionManager) UpdateSessionID(newID string) {
    sm.SessionID = newID
}

// SessionManagerInstance is a global instance of SessionManager
var SessionManagerInstance = &SessionManager{}

// Add these new vars at the top of the file
var (
    defaultTimeout = 30 * time.Second
    circuitBreaker *gobreaker.CircuitBreaker
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
}

type MorpheusSession struct {
    SessionID string
    ModelID   string    // Add ModelID field
    LastUsed  time.Time
}

// Add session management
var (
    activeSession *MorpheusSession
    sessionMutex  sync.Mutex
)

// Add model ID validation
func getModelID() string {
    modelID := os.Getenv("MODEL_ID")
    if modelID == "" {
        log.Fatal("MODEL_ID environment variable must be set")
    }
    return modelID
}

// ensureSession makes sure we have an active session with Morpheus node
func ensureSession() error {
    sessionMutex.Lock()
    defer sessionMutex.Unlock()

    // Debug the session state
    log.Printf("Checking session state - Current session: %+v", activeSession)

    if activeSession != nil && time.Since(activeSession.LastUsed) < 30*time.Minute {
        log.Printf("Using existing session: %s", activeSession.SessionID)
        return nil
    }

    modelID := getModelID()
    log.Printf("Establishing new session for model %s", modelID)

    // Updated session request structure with explicit duration value
    reqBody := map[string]interface{}{
        "sessionDuration": 3600, // Send as number, not string
        "failover": false,
    }

    reqBytes, err := json.Marshal(reqBody)
    if err != nil {
        return fmt.Errorf("failed to marshal session request: %v", err)
    }
    
    // Do a health check before establishing session
    healthResp, err := http.Get("http://marketplace:9000/healthcheck")
    if err != nil || healthResp.StatusCode != http.StatusOK {
        fmt.Printf("marketplace health check failed: %v", fmt.Errorf("%v", err))
    }

    // fmt.Printf("Health check status: %d\n", healthResp.StatusCode)
    // // Output health response body for debugging
    // bodyBytes, _ := io.ReadAll(healthResp.Body)
    // healthResp.Body.Close()
    // healthResp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
    // fmt.Printf("Health check response body: %s\n", string(bodyBytes))

    // Updated session endpoint with model ID
    sessionURL := fmt.Sprintf("http://marketplace:9000/blockchain/models/%s/session", modelID)
    resp, err := http.Post(sessionURL, "application/json", bytes.NewBuffer(reqBytes))
    if err != nil {
        log.Printf("Session establishment failed: %v", err)
        return fmt.Errorf("failed to establish session: %v", err)
    }
    
    // Read and log response body for debugging
    bodyBytes, _ := io.ReadAll(resp.Body)
    resp.Body.Close()
    resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
    log.Printf("Session response body: %s", string(bodyBytes))

    var result struct {
        Id string `json:"sessionID"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return err
    }

    log.Printf("Session response: %+v", result)

    activeSession = &MorpheusSession{
        SessionID: result.Id,
        ModelID:   modelID,
        LastUsed:  time.Now(),
    }

    if activeSession == nil || activeSession.SessionID == "" {
        return fmt.Errorf("failed to get valid session ID from response")
    }
    log.Printf("Successfully established new session: %s", activeSession.SessionID)
    return nil
}

// ProxyChatCompletion handles incoming chat completion requests
func ProxyChatCompletion(w http.ResponseWriter, r *http.Request) {
    // Read and log the request body
    bodyBytes, err := io.ReadAll(r.Body)
    if err != nil {
        respondWithError(w, http.StatusInternalServerError, "Failed to read request body")
        return
    }
    // Restore the request body for further processing
    r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
    
    fmt.Printf("Received chat request body: %s\n", string(bodyBytes))

    // Ensure we have active session
    if err := ensureSession(); err != nil {
        respondWithError(w, http.StatusInternalServerError, "Failed to establish session")
        return
    }

    var requestBody map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
        respondWithError(w, http.StatusBadRequest, "Invalid request body")
        return
    }

    // Always set MODEL_ID from environment
    modelID := os.Getenv("MODEL_ID")
    if modelID == "" {
        respondWithError(w, http.StatusInternalServerError, "MODEL_ID environment variable not set")
        return
    }
    requestBody["model"] = modelID

    // Add session_id to forwarded request headers
    r.Header.Set("session_id", activeSession.SessionID)
    
    stream, ok := requestBody["stream"].(bool)
    if (!ok) {
        stream = false // Default to non-streaming if not specified
    }

    if stream {
        handleStreamingRequest(w, requestBody)
    } else {
        handleNonStreamingRequest(w, requestBody)
    }
}

// forwardRequest forwards the request to the marketplace node with necessary headers
func forwardRequest(requestBody map[string]interface{}) (*http.Response, error) {
    marketplaceURL := os.Getenv("MARKETPLACE_URL")
    if marketplaceURL == "" {
        return nil, fmt.Errorf("MARKETPLACE_URL environment variable is not set")
    }

    // Add debug logging for URL
    log.Printf("Attempting to forward request to: %s", marketplaceURL)

    // Test marketplace connection
    client := &http.Client{Timeout: 5 * time.Second}
    _, err := client.Get(strings.TrimSuffix(marketplaceURL, "/v1/chat/completions"))
    if err != nil {
        return nil, fmt.Errorf("marketplace is not accessible: %v", err)
    }

    reqBodyBytes, err := json.Marshal(requestBody)
    if err != nil {
        return nil, fmt.Errorf("failed to marshal request body: %v", err)
    }

    req, err := http.NewRequest("POST", marketplaceURL, bytes.NewBuffer(reqBodyBytes))
    if err != nil {
        return nil, fmt.Errorf("failed to create request: %v", err)
    }

    req.Header.Set("Content-Type", "application/json")
    
 log.Printf("active session: %+v", activeSession)
    if activeSession != nil && activeSession.SessionID != "" {
        // Add session ID as both header variations to ensure compatibility
        req.Header.Set("Session_id", activeSession.SessionID)
        log.Printf("Setting session ID in request headers: %s", activeSession.SessionID)
    } else {
        log.Printf("Warning: No active session ID available")
        return nil, fmt.Errorf("no active session")
    }

    // Add debug logging for all headers
    log.Printf("Request headers: %v", req.Header)
    log.Printf("Request body: %s", reqBodyBytes)

    client = &http.Client{
        Timeout: 30 * time.Second,
    }

    // Add detailed error logging
    resp, err := client.Do(req)
    if err != nil {
        log.Printf("Request failed: %v", err)
        return nil, fmt.Errorf("failed to forward request: %v", err)
    }

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        log.Printf("Marketplace returned error status %d: %s", resp.StatusCode, string(body))
        resp.Body = io.NopCloser(bytes.NewBuffer(body))
    }

    // Add response logging
    log.Printf("Response status: %d", resp.StatusCode)
    log.Printf("Response headers: %v", resp.Header)

    return resp, nil
}

// handleStreamingRequest processes streaming requests
func handleStreamingRequest(w http.ResponseWriter, requestBody map[string]interface{}) {
    resp, err := forwardRequest(requestBody)
    if err != nil {
        respondWithError(w, http.StatusInternalServerError, "Failed to forward streaming request")
        return
    }
    defer resp.Body.Close()

    setStreamingHeaders(w)

    flusher, ok := w.(http.Flusher)
    if !ok {
        respondWithError(w, http.StatusInternalServerError, "Streaming unsupported")
        return
    }

    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        fmt.Fprintf(w, "%s\n", scanner.Text())
        flusher.Flush()
    }

    if err := scanner.Err(); err != nil {
        respondWithError(w, http.StatusInternalServerError, "Error reading streaming response")
    }
}

// handleNonStreamingRequest processes non-streaming requests
func handleNonStreamingRequest(w http.ResponseWriter, requestBody map[string]interface{}) {
    resp, err := forwardRequest(requestBody)
    if err != nil {
        respondWithError(w, http.StatusInternalServerError, "Failed to forward request")
        return
    }
    defer resp.Body.Close()

    copyHeaders(w, resp.Header)
    w.WriteHeader(resp.StatusCode)
    if _, err := io.Copy(w, resp.Body); err != nil {
        log.Printf("Error copying response body: %v", err)
    }
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

// StartProxyServer starts the proxy server
func StartProxyServer() {
    // Validate required environment variables
    walletAddress := os.Getenv("WALLET_ADDRESS")
    if walletAddress == "" || walletAddress == "0x0000000000000000000000000000000000000000" {
        log.Fatal("WALLET_ADDRESS environment variable must be set to a valid address")
    }
    
    modelID := getModelID() // Validate MODEL_ID exists
    log.Printf("Starting proxy server with Model ID: %s", modelID)

    http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
    })
    http.HandleFunc("/v1/chat/completions", ProxyChatCompletion)
    
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }
    log.Printf("Proxy server is running on port %s", port)
    log.Fatal(http.ListenAndServe(":"+port, nil))
}