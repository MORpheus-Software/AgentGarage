package proxy

import (
	"net/http"
	"sync"
	"time"
)

// ModelInfo represents information about a model
type ModelInfo struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

// CachedSession represents a cached session with its creation time
type CachedSession struct {
	SessionID  string
	ModelID    string
	ExpiresAt  time.Time
}

// CachedModel represents a cached model with its details
type CachedModel struct {
	ModelID   string
	ModelName string
	Created   time.Time
}

// MorpheusSession represents a session with the Morpheus API
type MorpheusSession struct {
	SessionID string
	ModelID   string
	Created   time.Time
}

// SessionManager manages sessions and their states
type SessionManager struct {
	sync.RWMutex
	SessionID string
	ModelID   string
}

// Proxy represents the proxy server configuration
type Proxy struct {
	client *http.Client
}

// ChatCompletionRequest represents a request for chat completion
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Model represents a model from the marketplace
type Model struct {
	Id   string `json:"id"`
	Name string `json:"name"`
} 
