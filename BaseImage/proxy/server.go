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
	"time"
)

// StartProxyServer starts the proxy server
func StartProxyServer() {
	proxy := NewProxy()

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	// Add handlers for blockchain/models endpoints
	http.HandleFunc("/blockchain/models", proxy.handleGetModels)
	http.HandleFunc("/blockchain/models/", proxy.handleModelOperations)
	http.HandleFunc("/v1/chat/completions", proxy.handleChatCompletions)

	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("DEFAULT_PORT")
		if port == "" {
			port = "8081"
		}
	}
	log.Printf("Proxy server is running on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Add handler for getting models
func (p *Proxy) handleGetModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	marketplaceURL := getMarketplaceModelsEndpoint()
	req, err := http.NewRequest(http.MethodGet, marketplaceURL, nil)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to fetch models", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// Add handler for model operations (session creation/deletion)
func (p *Proxy) handleModelOperations(w http.ResponseWriter, r *http.Request) {
	log.Printf("Handling model operation: %s %s", r.Method, r.URL.Path)
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/blockchain/models/"), "/")
	if len(pathParts) < 1 {
		log.Printf("Invalid path: %s", r.URL.Path)
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	marketplaceURL := fmt.Sprintf("%s/%s", getMarketplaceModelsEndpoint(), strings.Join(pathParts, "/"))
	log.Printf("Forwarding to marketplace URL: %s", marketplaceURL)

	// Forward the request to the marketplace
	req, err := http.NewRequest(r.Method, marketplaceURL, r.Body)
	if err != nil {
		log.Printf("Failed to create request: %v", err)
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy headers from original request
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	log.Printf("Request headers: %v", req.Header)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to forward request: %v", err)
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Log response details
	body, _ := io.ReadAll(resp.Body)
	log.Printf("Response status: %d", resp.StatusCode)
	log.Printf("Response body: %s", string(body))

	copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received chat completions request from %s", r.RemoteAddr)
	
	// Read and parse request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	log.Printf("Raw request body: %s", string(body))

	var chatRequest ChatCompletionRequest
	if err := json.Unmarshal(body, &chatRequest); err != nil {
		log.Printf("Error parsing chat request: %v", err)
		http.Error(w, "Error parsing request body", http.StatusBadRequest)
		return
	}

	// Ensure stream is set to true
	chatRequest.Stream = true
	
	// Check for existing session ID in header using consistent header name
	sessionID := r.Header.Get("session_id")
	log.Printf("Session ID from header: %s", sessionID)
	
	if sessionID != "" {
		sessionCache.RLock()
		if session, exists := sessionCache.m[sessionID]; exists && time.Now().Before(session.ExpiresAt) {
			sessionCache.RUnlock()
			log.Printf("Using existing session: %s for model %s", sessionID, session.ModelID)
			if err := p.forwardChatRequest(w, r, session.ModelID, chatRequest, sessionID); err != nil {
				log.Printf("Error forwarding chat request: %v", err)
				http.Error(w, fmt.Sprintf("Error forwarding request: %v", err), http.StatusInternalServerError)
			}
			return
		}
		sessionCache.RUnlock()
		log.Printf("Session %s not found or expired", sessionID)
	}

	// Get model ID from request
	modelID, err := validateModelHandle(chatRequest.Model)
	if err != nil {
		log.Printf("Error validating model handle: %v", err)
		http.Error(w, fmt.Sprintf("Error finding model ID: %v", err), http.StatusBadRequest)
		return
	}
	log.Printf("Validated model ID: %s", modelID)

	// Create new session
	sessionID, err = p.createSession(modelID)
	if err != nil {
		log.Printf("Error creating session: %v", err)
		http.Error(w, fmt.Sprintf("Error creating session: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Created new session: %s", sessionID)

	if err := p.forwardChatRequest(w, r, modelID, chatRequest, sessionID); err != nil {
		log.Printf("Error forwarding chat request: %v", err)
		http.Error(w, fmt.Sprintf("Error forwarding request: %v", err), http.StatusInternalServerError)
	}
}

func (p *Proxy) forwardChatRequest(w http.ResponseWriter, r *http.Request, modelID string, req ChatCompletionRequest, sessionID string) error {
	// Create request body with original model name (not ID) to match consumer node expectation
	reqBody := map[string]interface{}{
		"model":    req.Model, // Use original model name
		"messages": req.Messages,
		"stream":   true, // Always set to true
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("error marshaling request body: %v", err)
	}

	// Create the request
	endpoint := fmt.Sprintf("%s/v1/chat/completions", p.getMarketplaceBaseURL())
	proxyReq, err := http.NewRequest(r.Method, endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	// Set required headers
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Accept", "application/json")
	proxyReq.Header.Set("session_id", sessionID) // Use consistent session_id header

	// Log request details
	log.Printf("Forwarding request to: %s", endpoint)
	log.Printf("Request headers: %v", proxyReq.Header)
	log.Printf("Request body: %s", string(jsonBody))

	// Send the request with increased timeout
	client := &http.Client{
		Timeout: time.Minute * 5,
	}
	resp, err := client.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("marketplace request failed, status: %d, response: %s", resp.StatusCode, string(body))
	}

	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Stream the response
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading stream: %v", err)
		}
		if _, err := w.Write(line); err != nil {
			return fmt.Errorf("error writing stream: %v", err)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	return nil
} 