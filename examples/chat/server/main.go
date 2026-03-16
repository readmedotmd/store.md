// Chat server - persists messages and syncs with browsers.
//
// ## Run:
//
//	cd server
//	go run main.go
//
// ## Build client (in another terminal):
//
//	cd client
//	GOOS=js GOARCH=wasm go build -o chat.wasm
//
// Then open http://localhost:8090 in multiple browsers

//go:build !js || !wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/readmedotmd/store.md/backend/bbolt"
	"github.com/readmedotmd/store.md/sync/core"
	"github.com/readmedotmd/store.md/sync/server"
)

// wasmExecJS is loaded at startup from GOROOT
var wasmExecJS []byte

func init() {
	// Find GOROOT
	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
			goroot = strings.TrimSpace(string(out))
		}
	}
	if goroot == "" {
		log.Fatal("GOROOT not found. Set GOROOT environment variable or ensure 'go' is in PATH")
	}

	// Load wasm_exec.js (try lib/wasm first, then misc/wasm)
	var err error
	wasmExecJS, err = os.ReadFile(goroot + "/lib/wasm/wasm_exec.js")
	if err != nil {
		wasmExecJS, err = os.ReadFile(goroot + "/misc/wasm/wasm_exec.js")
		if err != nil {
			log.Fatalf("wasm_exec.js not found in %s/lib/wasm/ or %s/misc/wasm/: %v", goroot, goroot, err)
		}
	}
}

// ChatMessage represents a persisted chat message
type ChatMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

func main() {
	// Create data directory
	os.MkdirAll("data", 0755)

	// Use BBolt for persistence - messages survive server restarts
	store, err := bbolt.New("data/chat.db")
	if err != nil {
		log.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// Create sync store
	ss := core.New(store)
	defer ss.Close()

	// Simple auth - peer ID from query param
	auth := func(r *http.Request) (string, error) {
		peerID := r.URL.Query().Get("peer")
		if peerID == "" {
			peerID = fmt.Sprintf("peer-%d", time.Now().UnixNano())
		}
		return peerID, nil
	}

	// Create HTTP mux
	mux := http.NewServeMux()

	// Serve static files
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/wasm_exec.js", serveWasmExec)
	mux.HandleFunc("/chat.wasm", serveWasm)

	// API: Get all messages (for initial load)
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// List all messages from store
		pairs, err := store.List(ctx, struct {
			Prefix     string
			StartAfter string
			Limit      int
		}{Prefix: "msg/"})

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var messages []ChatMessage
		for _, p := range pairs {
			var msg ChatMessage
			if err := json.Unmarshal([]byte(p.Value), &msg); err == nil {
				messages = append(messages, msg)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(messages)
	})

	// Create sync server
	srv := server.New(ss, auth)
	srv.SetLogger(slog.Default())
	srv.SetAllowedOrigins([]string{"*"})

	// Mount sync server at /sync
	mux.Handle("/sync", srv)
	mux.Handle("/sync/", srv)

	// Log all requests for debugging
	loggedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.Method, r.URL.Path, r.UserAgent())
		mux.ServeHTTP(w, r)
	})

	// Start server
	addr := ":8090"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: loggedMux,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()

	log.Printf("Chat server starting on http://localhost%s", addr)
	log.Printf("Open http://localhost%s in multiple browser tabs", addr)

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
}

func serveWasmExec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Write(wasmExecJS)
}

func serveWasm(w http.ResponseWriter, r *http.Request) {
	// The WASM file should be built separately
	data, err := os.ReadFile("client/chat.wasm")
	if err != nil {
		http.Error(w, "WASM not built. Run: cd client && GOOS=js GOARCH=wasm go build -o chat.wasm", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/wasm")
	w.Write(data)
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>store.md Chat</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: #f5f5f5;
            height: 100vh;
            display: flex;
            flex-direction: column;
        }
        .app {
            display: flex;
            flex-direction: column;
            height: 100vh;
        }
        .header {
            background: #2c3e50;
            color: white;
            padding: 1rem;
            text-align: center;
        }
        .header h1 { font-size: 1.25rem; font-weight: 600; }
        .status {
            font-size: 0.8rem;
            opacity: 0.8;
            margin-top: 0.25rem;
        }
        .status.connected { color: #2ecc71; }
        .status.syncing { color: #f39c12; }
        .status.disconnected { color: #e74c3c; }
        .messages {
            flex: 1;
            overflow-y: auto;
            padding: 1rem;
            display: flex;
            flex-direction: column;
            gap: 0.75rem;
        }
        .message {
            max-width: 80%;
            padding: 0.75rem 1rem;
            border-radius: 1rem;
        }
        .message.own {
            align-self: flex-end;
            background: #3498db;
            color: white;
            border-bottom-right-radius: 0.25rem;
        }
        .message.other {
            align-self: flex-start;
            background: white;
            color: #333;
            border-bottom-left-radius: 0.25rem;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .message .from {
            font-size: 0.75rem;
            opacity: 0.7;
            margin-bottom: 0.25rem;
        }
        .message .content { line-height: 1.4; }
        .message .time {
            font-size: 0.7rem;
            opacity: 0.6;
            margin-top: 0.25rem;
            text-align: right;
        }
        .input-area {
            background: white;
            padding: 1rem;
            border-top: 1px solid #e0e0e0;
            display: flex;
            gap: 0.75rem;
        }
        .input {
            flex: 1;
            padding: 0.75rem 1rem;
            border: 1px solid #ddd;
            border-radius: 2rem;
            font-size: 1rem;
            outline: none;
        }
        .input:focus { border-color: #3498db; }
        .send-btn {
            padding: 0.75rem 1.5rem;
            background: #3498db;
            color: white;
            border: none;
            border-radius: 2rem;
            font-size: 1rem;
            cursor: pointer;
        }
        .send-btn:hover { background: #2980b9; }
        .status-bar {
            display: flex;
            align-items: center;
            justify-content: center;
            gap: 1rem;
            margin-top: 0.5rem;
        }
        .conn-btn {
            padding: 0.25rem 0.75rem;
            background: #34495e;
            color: white;
            border: none;
            border-radius: 0.25rem;
            font-size: 0.75rem;
            cursor: pointer;
        }
        .conn-btn:hover { background: #2c3e50; }
        #loading {
            position: fixed;
            top: 0; left: 0; right: 0; bottom: 0;
            background: white;
            display: flex;
            align-items: center;
            justify-content: center;
            z-index: 100;
        }
        .spinner {
            width: 40px;
            height: 40px;
            border: 3px solid #f3f3f3;
            border-top: 3px solid #3498db;
            border-radius: 50%;
            animation: spin 1s linear infinite;
        }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
    </style>
</head>
<body>
    <div id="loading">
        <div class="spinner"></div>
    </div>
    <div id="app"></div>
    <script src="/wasm_exec.js"></script>
    <script>
        const go = new Go();
        WebAssembly.instantiateStreaming(fetch("/chat.wasm"), go.importObject).then((result) => {
            go.run(result.instance);
            document.getElementById('loading').style.display = 'none';
        }).catch(err => {
            document.getElementById('loading').innerHTML = '<div style="color:red">Error: ' + err.message + '</div>';
            console.error(err);
        });
    </script>
</body>
</html>`
