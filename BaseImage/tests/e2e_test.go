package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MORpheusSoftware/NFA/BaseImage/mocks"
)

func StartMockServer(ctx context.Context, wg *sync.WaitGroup, port string, t *testing.T) {
	defer wg.Done()
	server := &http.Server{Addr: ":" + port, Handler: http.HandlerFunc(mocks.MockMarketplaceHandler)}
	serverErrors := make(chan error, 1)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrors <- fmt.Errorf("Mock Marketplace Server ListenAndServe: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		// Context canceled, proceed to shutdown
	case err := <-serverErrors:
		t.Fatalf("Mock Marketplace Server error: %v", err)
	}

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctxShutDown); err != nil {
		t.Fatalf("Mock Marketplace Server Shutdown Failed:%+v", err)
	}
}

func TestProxyServerOpenAICompatibility(t *testing.T) {
	// Read environment variables
	proxyServerURL := os.Getenv("PROXY_SERVER_URL")
	if proxyServerURL == "" {
		proxyServerURL = "http://localhost:8080"
	}

	marketplaceURL := os.Getenv("MARKETPLACE_URL")
	if marketplaceURL == "" {
		marketplaceURL = "http://localhost:9000/v1/chat/completions"
		// Start the mock marketplace server if necessary

		var wg sync.WaitGroup
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if strings.Contains(marketplaceURL, "localhost:9000") {
			wg.Add(1)
			go StartMockServer(ctx, &wg, "9000", t)
			// Allow the mock server to start
			time.Sleep(1 * time.Second)
		}
	}

	// Set up environment variables for the test if needed
	os.Setenv("MARKETPLACE_URL", marketplaceURL)
	defer os.Unsetenv("MARKETPLACE_URL")

	// Define test cases
	testCases := []struct {
		name           string
		requestBody    map[string]interface{}
		expectedStatus int
		validateFunc   func(t *testing.T, resp *http.Response)
	}{
		{
			name: "Non-Streaming Request",
			requestBody: map[string]interface{}{
				"model":    "gpt-3.5-turbo",
				"messages": []map[string]string{{"role": "user", "content": "Hello"}},
			},
			expectedStatus: http.StatusOK,
			validateFunc: func(t *testing.T, resp *http.Response) {
				var response map[string]interface{}
				if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
					t.Fatalf("Failed to decode response: %v", err)
				}

				if response["object"] != "text_completion" {
					t.Errorf("Expected object 'text_completion', got '%v'", response["object"])
				}

				choices, ok := response["choices"].([]interface{})
				if !ok || len(choices) == 0 {
					t.Errorf("Expected non-empty choices, got '%v'", response["choices"])
				}

				firstChoice, ok := choices[0].(map[string]interface{})
				if !ok {
					t.Errorf("Expected first choice to be a map, got '%T'", choices[0])
				}

				text, ok := firstChoice["text"].(string)
				if !ok || text != "Hello world!" {
					t.Errorf("Expected text 'Hello world!', got '%v'", firstChoice["text"])
				}
			},
		},
		{
			name: "Streaming Request",
			requestBody: map[string]interface{}{
				"model":    "gpt-3.5-turbo",
				"messages": []map[string]string{{"role": "user", "content": "Hello"}},
				"stream":   true,
			},
			expectedStatus: http.StatusOK,
			validateFunc: func(t *testing.T, resp *http.Response) {
				// Read the streaming response
				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Failed to read response body: %v", err)
				}

				bodyString := string(bodyBytes)

				if !strings.Contains(bodyString, "Hello") || !strings.Contains(bodyString, "world!") {
					t.Errorf("Streaming response does not contain expected messages. Got: %s", bodyString)
				}
			},
		},
		{
			name: "Missing session_id",
			requestBody: map[string]interface{}{
				"model":    "gpt-3.5-turbo",
				"messages": []map[string]string{{"role": "user", "content": "Hello"}},
			},
			// This test assumes the proxy requires a session_id header
			expectedStatus: http.StatusUnauthorized,
			validateFunc: func(t *testing.T, resp *http.Response) {
				var response map[string]interface{}
				if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
					t.Fatalf("Failed to decode response: %v", err)
				}

				errorMsg, ok := response["error"].(string)
				if !ok || !strings.Contains(errorMsg, "Missing session_id") {
					t.Errorf("Expected error message containing 'Missing session_id', got '%v'", response["error"])
				}
			},
		},
		{
			name:           "Invalid JSON",
			requestBody:    nil, // Send invalid JSON
			expectedStatus: http.StatusBadRequest,
			validateFunc: func(t *testing.T, resp *http.Response) {
				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Failed to read response body: %v", err)
				}

				bodyString := string(bodyBytes)
				if !strings.Contains(bodyString, "Invalid request body") {
					t.Errorf("Expected error message 'Invalid request body', got '%s'", bodyString)
				}
			},
		},
	}

	// Execute test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var reqBody io.Reader
			if tc.requestBody != nil {
				reqBytes, err := json.Marshal(tc.requestBody)
				if err != nil {
					t.Fatalf("Failed to marshal request body: %v", err)
				}
				reqBody = bytes.NewReader(reqBytes)
			} else {
				// Send invalid JSON
				reqBody = bytes.NewReader([]byte("{invalid-json"))
			}

			// Create a new HTTP request to the proxy server
			req, err := http.NewRequest("POST", proxyServerURL+"/v1/chat/completions", reqBody)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			// Add headers
			req.Header.Set("Content-Type", "application/json")
			if tc.name != "Missing session_id" {
				req.Header.Set("session_id", "test_session_id")
			}

			// Perform the request
			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Failed to perform request: %v", err)
			}
			defer resp.Body.Close()

			// Check status code
			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d", tc.expectedStatus, resp.StatusCode)
			}

			// Validate response
			tc.validateFunc(t, resp)
		})
	}

	// Wait for the mock server to shut down if it was started
	wg.Wait()
}

type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func TestChatCompletionEndToEnd(t *testing.T) {
	// Test configuration
	proxyURL := "http://localhost:8080"
	modelID := "test-model"
	numRequests := 5
	var wg sync.WaitGroup

	// Create a chat completion request
	request := ChatCompletionRequest{
		Model: modelID,
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
		Stream: true,
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Send multiple concurrent requests
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(requestNum int) {
			defer wg.Done()

			// Create a new request
			req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1/chat/completions", proxyURL), bytes.NewBuffer(requestBody))
			if err != nil {
				t.Errorf("Failed to create request %d: %v", requestNum, err)
				return
			}

			// Set headers
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")

			// Send the request
			client := &http.Client{
				Timeout: time.Minute * 5,
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("Failed to send request %d: %v", requestNum, err)
				return
			}
			defer resp.Body.Close()

			// Check response status
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("Request %d failed with status %d: %s", requestNum, resp.StatusCode, string(body))
				return
			}

			// Read and validate streaming response
			reader := bufio.NewReader(resp.Body)
			eventCount := 0
			for {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					if err == io.EOF {
						break
					}
					t.Errorf("Error reading stream in request %d: %v", requestNum, err)
					return
				}

				// Skip empty lines
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}

				// Parse and validate the event
				if !bytes.HasPrefix(line, []byte("data: ")) {
					t.Errorf("Invalid event format in request %d: %s", requestNum, string(line))
					continue
				}

				eventCount++
			}

			if eventCount == 0 {
				t.Errorf("Request %d received no events", requestNum)
			}
		}(i)
	}

	// Wait for all requests to complete
	wg.Wait()
}
