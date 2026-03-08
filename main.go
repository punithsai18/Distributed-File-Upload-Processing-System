package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

//go:embed frontend/dist
var frontendDist embed.FS

// FileStatus tracks the processing state of an uploaded file.
type FileStatus struct {
	Hash       string    `json:"hash"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	Status     string    `json:"status"` // queued | processing | done | duplicate | error
	Worker     string    `json:"worker,omitempty"`
	Message    string    `json:"message,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	DoneAt     time.Time `json:"done_at,omitempty"`
}

// UploadResponse is the JSON body returned to the client after upload.
type UploadResponse struct {
	Hash       string    `json:"hash"`
	Message    string    `json:"message"`
	Status     string    `json:"status"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
}

var (
	etcdClient *clientv3.Client
	mongoDB    *mongo.Database
	gridBucket *gridfs.Bucket

	storeMu sync.RWMutex
	// Algorithm 3: Content-Addressable Storage. We index files by their SHA-256 hash
	// instead of filenames. This ensures deduplication and data integrity.
	fileStore = make(map[string]*FileStatus) // hash → status

	workQueue = make(chan *FileStatus, 100)
	uploadDir = "./uploads"

	hostToken string // set from HOST_TOKEN env var; empty = delete disabled

	// Leader election state — updated by runLeaderElection goroutines.
	leaderStateMu sync.RWMutex
	currentLeader string
)

// ─── MongoDB helpers ────────────────────────────────────────────────────────────

// mongoSaveMetadata upserts a FileStatus document in the "files" collection.
func mongoSaveMetadata(entry *FileStatus) {
	if mongoDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	filter := bson.M{"hash": entry.Hash}
	update := bson.M{"$set": entry}
	opts := options.Update().SetUpsert(true)
	if _, err := mongoDB.Collection("files").UpdateOne(ctx, filter, update, opts); err != nil {
		log.Printf("⚠  MongoDB: failed to save metadata for %s: %v", entry.Hash[:12], err)
	}
}

// mongoSaveFile stores the raw file content in GridFS using the hash as filename.
func mongoSaveFile(hash, filename string, content []byte) {
	if gridBucket == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Delete any existing file with the same hash name to avoid duplicates.
	cursor, err := gridBucket.Find(bson.M{"filename": hash})
	if err == nil {
		var results []bson.M
		if err := cursor.All(ctx, &results); err == nil {
			for _, r := range results {
				if id, ok := r["_id"]; ok {
					_ = gridBucket.Delete(id)
				}
			}
		}
	}

	// Algorithm 5: GridFS Chunking. MongoDB splits the binary data into 255KB chunks
	// stored across fs.files and fs.chunks collections.
	uploadStream, err := gridBucket.OpenUploadStream(hash,
		options.GridFSUpload().SetMetadata(bson.M{"originalName": filename}))
	if err != nil {
		log.Printf("⚠  GridFS: failed to open upload stream for %s: %v", hash[:12], err)
		return
	}
	defer uploadStream.Close()

	if _, err := uploadStream.Write(content); err != nil {
		log.Printf("⚠  GridFS: failed to write content for %s: %v", hash[:12], err)
	}
}

// ─── startup restore ───────────────────────────────────────────────────────────

// restoreFileStore re-populates fileStore from MongoDB (if available) and
// then from files on disk so that uploads survive container restarts.
func restoreFileStore() {
	// First, restore from MongoDB if connected.
	if mongoDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cursor, err := mongoDB.Collection("files").Find(ctx, bson.M{})
		if err != nil {
			log.Printf("⚠  MongoDB: could not read files collection: %v", err)
		} else {
			var docs []FileStatus
			if err := cursor.All(ctx, &docs); err != nil {
				log.Printf("⚠  MongoDB: could not decode files: %v", err)
			} else {
				storeMu.Lock()
				for i := range docs {
					fileStore[docs[i].Hash] = &docs[i]
				}
				storeMu.Unlock()
				if len(docs) > 0 {
					log.Printf("♻  Restored %d file(s) from MongoDB", len(docs))
				}
			}
		}
	}

	// Then, scan disk for any files not already in the store.
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		log.Printf("⚠  Could not read upload directory: %v", err)
		return
	}
	count := 0
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		// Files are saved as "<hash>_<original-filename>".
		hash, filename, found := strings.Cut(name, "_")
		if !found {
			log.Printf("⚠  Skipping unrecognized file in upload directory: %s", name)
			continue
		}
		info, err := de.Info()
		if err != nil {
			log.Printf("⚠  Could not stat upload file %s: %v", name, err)
			continue
		}
		storeMu.Lock()
		if _, exists := fileStore[hash]; !exists {
			fileStore[hash] = &FileStatus{
				Hash:     hash,
				Filename: filename,
				Size:     info.Size(),
				Status:   "done",
			}
			count++
		}
		storeMu.Unlock()
	}
	if count > 0 {
		log.Printf("♻  Restored %d file(s) from %s", count, uploadDir)
	}
}

// ─── Leader Election ──────────────────────────────────────────────────────────

// runLeaderElection campaigns for cluster leadership using etcd's election API.
// Exactly one candidate wins the election and becomes the coordinator; the
// identity of the current leader is stored in currentLeader and exposed via
// the /health endpoint.  If etcd is unavailable the function returns
// immediately, leaving currentLeader empty.  When ctx is cancelled the leader
// resigns gracefully before returning.
func runLeaderElection(ctx context.Context, candidateID string) {
	if etcdClient == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sess, err := concurrency.NewSession(etcdClient,
			concurrency.WithContext(ctx),
			concurrency.WithTTL(15))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[%s] leader-election: session error: %v — retrying in 5s", candidateID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// Algorithm 4: Leader Election via etcd.
		// All nodes "campaign" for the coordinator role. One wins, others wait.
		election := concurrency.NewElection(sess, "/election/coordinator")
		log.Printf("[%s] 🗳  campaigning for leadership", candidateID)

		// Campaign blocks until this candidate is elected or ctx is done.
		if err := election.Campaign(ctx, candidateID); err != nil {
			sess.Close()
			if ctx.Err() != nil {
				return
			}
			log.Printf("[%s] leader-election: campaign error: %v — retrying in 5s", candidateID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// This candidate is now the elected leader.
		leaderStateMu.Lock()
		currentLeader = candidateID
		leaderStateMu.Unlock()
		log.Printf("[%s] 👑 elected as cluster leader", candidateID)

		// Hold leadership until the session expires or the context is cancelled.
		select {
		case <-ctx.Done():
			// Resign gracefully so another candidate can take over immediately.
			// Use context.Background() with a short timeout instead of the
			// cancelled ctx so the resign RPC can still reach etcd during shutdown.
			resignCtx, resignCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := election.Resign(resignCtx); err != nil {
				log.Printf("[%s] leader-election: resign error: %v", candidateID, err)
			}
			resignCancel()
			sess.Close()
			leaderStateMu.Lock()
			if currentLeader == candidateID {
				currentLeader = ""
			}
			leaderStateMu.Unlock()
			log.Printf("[%s] 📤 resigned leadership (shutdown)", candidateID)
			return

		case <-sess.Done():
			// Session lease expired — lost leadership; re-campaign.
			leaderStateMu.Lock()
			if currentLeader == candidateID {
				currentLeader = ""
			}
			leaderStateMu.Unlock()
			log.Printf("[%s] ⚠  session expired — lost leadership, re-campaigning", candidateID)
			sess.Close()
		}
	}
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("Cannot create upload directory: %v", err)
	}

	hostToken = os.Getenv("HOST_TOKEN")

	// Connect to MongoDB (optional – system degrades gracefully without it).
	// MONGO_URI accepts any valid MongoDB connection string, including
	// MongoDB Atlas SRV URIs (mongodb+srv://user:pass@cluster.mongodb.net/).
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	// Use a generous connect timeout: Atlas SRV URIs require a DNS lookup
	// and TLS handshake which can take several seconds on first connection.
	mongoDBName := os.Getenv("MONGO_DB")
	if mongoDBName == "" {
		mongoDBName = "dscasestudy"
	}
	// 15 s covers Atlas SRV DNS lookup + TLS handshake + geographic latency
	// to the nearest Atlas cluster region. Local MongoDB typically connects
	// in under 100 ms so this timeout has no practical cost in that case.
	mongoCtx, mongoCancel := context.WithTimeout(context.Background(), 15*time.Second)
	mongoClient, err := mongo.Connect(mongoCtx, options.Client().ApplyURI(mongoURI))
	mongoCancel()
	if err != nil {
		log.Printf("⚠  MongoDB unavailable: %v — running without MongoDB", err)
	} else {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
		pingErr := mongoClient.Ping(pingCtx, nil)
		pingCancel()
		if pingErr != nil {
			log.Printf("⚠  MongoDB not reachable (%v) — running without MongoDB", pingErr)
			_ = mongoClient.Disconnect(context.Background())
		} else {
			log.Printf("✔  Connected to MongoDB (db: %s)", mongoDBName)
			mongoDB = mongoClient.Database(mongoDBName)
			var bucketErr error
			gridBucket, bucketErr = gridfs.NewBucket(mongoDB)
			if bucketErr != nil {
				log.Printf("⚠  GridFS bucket error: %v", bucketErr)
				gridBucket = nil
			}
			defer mongoClient.Disconnect(context.Background()) //nolint:errcheck
		}
	}

	// Restore fileStore from MongoDB and/or previously saved files on disk.
	restoreFileStore()

	// Determine etcd endpoint (env var takes priority over default).
	etcdEndpoint := os.Getenv("ETCD_ENDPOINT")
	if etcdEndpoint == "" {
		etcdEndpoint = "localhost:2379"
	}

	// Connect to etcd (optional – system degrades gracefully without it).
	var etcdErr error
	etcdClient, etcdErr = clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdEndpoint},
		DialTimeout: 5 * time.Second,
		Logger:      zap.NewNop(), // suppress gRPC internal warnings
	})
	if etcdErr != nil {
		log.Printf("⚠  etcd unavailable: %v — running in standalone mode", etcdErr)
		etcdClient = nil
	} else {
		// Verify connectivity with a short ping.
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, pingErr := etcdClient.Status(pingCtx, etcdEndpoint)
		pingCancel()
		if pingErr != nil {
			log.Printf("⚠  etcd not reachable (%v) — running in standalone mode", pingErr)
			etcdClient.Close()
			etcdClient = nil
		} else {
			log.Printf("✔  Connected to etcd at %s", etcdEndpoint)
			defer etcdClient.Close()
		}
	}

	// Algorithm 7: Worker Pool Pattern.
	// We spawn a fixed number of goroutines to handle background processing.
	const numWorkers = 3
	for i := 1; i <= numWorkers; i++ {
		go runWorker(fmt.Sprintf("worker-%d", i))
	}

	// Each worker campaigns for cluster leadership via etcd leader election.
	// The elected leader is tracked in currentLeader and exposed on /health.
	electionCtx, electionCancel := context.WithCancel(context.Background())
	defer electionCancel()
	for i := 1; i <= numWorkers; i++ {
		go runLeaderElection(electionCtx, fmt.Sprintf("worker-%d", i))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", cors(handleUpload))
	mux.HandleFunc("/status/", cors(handleStatus))
	mux.HandleFunc("/files", cors(handleListFiles))
	mux.HandleFunc("/download/", cors(handleDownload))
	mux.HandleFunc("/delete/", cors(handleDeleteFile))
	mux.HandleFunc("/health", cors(handleHealth))

	// Serve the built React frontend (embedded in the binary).
	// Any path not matched by the API routes above falls through to here.
	// If the requested file doesn't exist or is a directory (e.g. a React
	// client-side route), serve index.html so the browser loads the SPA.
	distSub, err := fs.Sub(frontendDist, "frontend/dist")
	if err != nil {
		log.Fatalf("Cannot access embedded frontend: %v", err)
	}
	distFS := http.FileServer(http.FS(distSub))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		f, err := distSub.Open("index.html")
		if err != nil {
			http.Error(w, "frontend not available", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "frontend not available", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "index.html", stat.ModTime(), f.(io.ReadSeeker))
	}
	mux.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		// fs.FS paths must not start with '/'; strip it before opening.
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if urlPath == "" {
			urlPath = "."
		}
		// Try opening the requested path in the embedded FS.
		f, err := distSub.Open(urlPath)
		if err != nil {
			// File not found — fall back to index.html for SPA routing.
			serveIndex(w, r)
			return
		}
		stat, err := f.Stat()
		f.Close()
		if err != nil || stat.IsDir() {
			// Directory paths also fall back to index.html.
			serveIndex(w, r)
			return
		}
		distFS.ServeHTTP(w, r)
	}))

	// Determine listen port (env var takes priority over default).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	log.Printf("🚀  Server listening on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ─── HTTP handlers ─────────────────────────────────────────────────────────────

// jsonError writes a JSON-encoded error response so the browser's
// res.json() call never sees plain-text or an empty body.
func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		log.Printf("⚠  jsonError: failed to encode error response: %v", err)
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit upload size to 50 MB.
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		jsonError(w, "file too large", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read content and compute SHA-256 hash.
	content, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	// Algorithm 3: SHA-256 Hashing. Read all bytes and produce a unique hash.
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	// Deduplication check.
	storeMu.Lock()
	if existing, ok := fileStore[hash]; ok {
		storeMu.Unlock()
		log.Printf("⚠  Duplicate detected! Hash %s already exists (status: %s, filename: %s)", hash[:12], existing.Status, existing.Filename)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(UploadResponse{
			Hash:       hash,
			Status:     "duplicate",
			Message:    fmt.Sprintf("Duplicate of '%s' (already %s)", existing.Filename, existing.Status),
			UploadedAt: existing.UploadedAt,
		}); err != nil {
			log.Printf("⚠  handleUpload: failed to encode duplicate response: %v", err)
		}
		return
	}

	// Persist file to disk.
	dest := filepath.Join(uploadDir, hash+"_"+header.Filename)
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		storeMu.Unlock()
		jsonError(w, "cannot save file", http.StatusInternalServerError)
		return
	}

	entry := &FileStatus{
		Hash:       hash,
		Filename:   header.Filename,
		Size:       int64(len(content)),
		Status:     "queued",
		UploadedAt: time.Now().UTC(),
	}
	fileStore[hash] = entry
	storeMu.Unlock()

	// Save file content to GridFS and metadata to MongoDB.
	go mongoSaveFile(hash, header.Filename, content)
	go mongoSaveMetadata(entry)

	// Enqueue for processing.
	workQueue <- entry

	log.Printf("📥  Received '%s' → hash %s", header.Filename, hash[:12])

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(UploadResponse{
		Hash:       hash,
		Status:     "queued",
		Message:    "File queued for processing",
		UploadedAt: entry.UploadedAt,
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Path[len("/status/"):]
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}

	storeMu.RLock()
	entry, ok := fileStore[hash]
	storeMu.RUnlock()

	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entry)
}

func handleListFiles(w http.ResponseWriter, r *http.Request) {
	storeMu.RLock()
	list := make([]*FileStatus, 0, len(fileStore))
	for _, v := range fileStore {
		list = append(list, v)
	}
	storeMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleDownload serves the file identified by its SHA-256 hash.
// It first tries GridFS (MongoDB), then falls back to the local upload directory.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/download/")
	if hash == "" {
		jsonError(w, "missing hash", http.StatusBadRequest)
		return
	}

	storeMu.RLock()
	entry, ok := fileStore[hash]
	storeMu.RUnlock()
	if !ok {
		jsonError(w, "file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Filename))
	w.Header().Set("Content-Type", "application/octet-stream")

	// Try to serve from GridFS first.
	if gridBucket != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stream, err := gridBucket.OpenDownloadStreamByName(hash)
		if err == nil {
			defer stream.Close()
			if _, copyErr := io.Copy(w, stream); copyErr != nil {
				log.Printf("⚠  handleDownload: GridFS copy error for %s: %v", hash[:12], copyErr)
				_ = ctx // keep linter happy
			}
			return
		}
		log.Printf("⚠  handleDownload: GridFS miss for %s (%v), falling back to disk", hash[:12], err)
	}

	// Fall back to disk.
	diskPath := filepath.Join(uploadDir, hash+"_"+entry.Filename)
	f, err := os.Open(diskPath) //nolint:gosec
	if err != nil {
		jsonError(w, "file not available", http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		jsonError(w, "file not available", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, entry.Filename, stat.ModTime(), f)
}

// handleDeleteFile removes a file from fileStore, MongoDB, GridFS, and disk.
// Only accessible when HOST_TOKEN is configured and the caller provides it via
// the X-Host-Token request header.
func handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Algorithm 6: Constant-Time Comparison.
	// Prevents timing attacks by comparing tokens character-by-character in a fixed time.
	if hostToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Host-Token")), []byte(hostToken)) != 1 {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	hash := strings.TrimPrefix(r.URL.Path, "/delete/")
	if hash == "" {
		jsonError(w, "missing hash", http.StatusBadRequest)
		return
	}

	storeMu.Lock()
	entry, ok := fileStore[hash]
	if ok {
		delete(fileStore, hash)
	}
	storeMu.Unlock()

	if !ok {
		jsonError(w, "file not found", http.StatusNotFound)
		return
	}

	// Remove from disk.
	diskPath := filepath.Join(uploadDir, hash+"_"+entry.Filename)
	if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
		log.Printf("⚠  handleDeleteFile: could not remove disk file %s: %v", diskPath, err)
	}

	// Remove from GridFS.
	if gridBucket != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cursor, err := gridBucket.Find(bson.M{"filename": hash})
		if err == nil {
			var results []bson.M
			if err := cursor.All(ctx, &results); err == nil {
				for _, res := range results {
					if id, ok := res["_id"]; ok {
						_ = gridBucket.Delete(id)
					}
				}
			}
		}
	}

	// Remove metadata from MongoDB.
	if mongoDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = mongoDB.Collection("files").DeleteOne(ctx, bson.M{"hash": hash})
	}

	hashPreview := hash
	if len(hashPreview) > 12 {
		hashPreview = hashPreview[:12]
	}
	log.Printf("🗑  Deleted file '%s' (hash %s)", entry.Filename, hashPreview)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "hash": hash}); err != nil {
		log.Printf("⚠  handleDeleteFile: failed to encode response: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	etcdOK := etcdClient != nil
	mongoOK := mongoDB != nil

	leaderStateMu.RLock()
	leader := currentLeader
	leaderStateMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"etcd":      etcdOK,
		"mongo":     mongoOK,
		"workers":   3,
		"leader":    leader,
		"timestamp": time.Now().UTC(),
	})
}

// ─── Worker ────────────────────────────────────────────────────────────────────

func runWorker(workerID string) {
	log.Printf("[%s] started", workerID)
	for entry := range workQueue {
		processFile(workerID, entry)
	}
}

func processFile(workerID string, entry *FileStatus) {
	lockKey := "/locks/" + entry.Hash
	usingLock := false

	if etcdClient != nil {
		// Create an etcd session (TTL 30 s so the lock auto-expires on crash).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		sess, err := concurrency.NewSession(etcdClient, concurrency.WithContext(ctx), concurrency.WithTTL(30))
		if err != nil {
			log.Printf("[%s] etcd session unavailable (%v) — falling back to standalone", workerID, err)
		} else {
			defer sess.Close()
			// Algorithm 2: Distributed Locking (etcd Mutex).
			// This ensures "Exactly-Once" processing across the whole cluster.
			// Powered by Raft consensus internally in etcd.
			mu := concurrency.NewMutex(sess, lockKey)
			lockCtx, lockCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer lockCancel()

			log.Printf("[%s] ⏳ acquiring lock %s", workerID, lockKey)
			if err := mu.Lock(lockCtx); err != nil {
				log.Printf("[%s] lock error: %v — falling back to standalone", workerID, err)
			} else {
				usingLock = true
				log.Printf("[%s] 🔒 lock acquired for hash %s", workerID, entry.Hash[:12])

				// Re-check status after acquiring lock.
				// Another worker might have finished it while we were waiting for the lock.
				storeMu.RLock()
				currentStatus := entry.Status
				storeMu.RUnlock()

				if currentStatus == "done" || currentStatus == "duplicate" {
					log.Printf("[%s] ⏭  Skipping: Hash %s already processed by another worker", workerID, entry.Hash[:12])
					mu.Unlock(context.Background())
					log.Printf("[%s] 🔓 lock released for hash %s (skipped)", workerID, entry.Hash[:12])
					return
				}

				defer func() {
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer releaseCancel()
					mu.Unlock(releaseCtx) //nolint:errcheck
					log.Printf("[%s] 🔓 lock released for hash %s", workerID, entry.Hash[:12])
				}()
			}
		}
	}

	if !usingLock {
		storeMu.RLock()
		currentStatus := entry.Status
		storeMu.RUnlock()
		if currentStatus == "done" || currentStatus == "duplicate" {
			log.Printf("[%s] ⏭  Skipping: Hash %s already processed (standalone mode)", workerID, entry.Hash[:12])
			return
		}
		log.Printf("[%s] processing %s (standalone mode)", workerID, entry.Hash[:12])
	}

	// Log when the elected leader is the one doing this processing — purely
	// informational so operators can see which worker holds coordinator status.
	leaderStateMu.RLock()
	isCoordinator := currentLeader == workerID
	leaderStateMu.RUnlock()
	if isCoordinator {
		log.Printf("[%s] 👑 processing as elected cluster leader", workerID)
	}

	// Mark as processing.
	storeMu.Lock()
	entry.Status = "processing"
	entry.Worker = workerID
	entry.StartedAt = time.Now().UTC()
	storeMu.Unlock()

	log.Printf("[%s] ⚙  processing '%s'", workerID, entry.Filename)

	// Simulate real work (virus scan, thumbnail, compression, …).
	time.Sleep(3 * time.Second)

	// Mark as done.
	storeMu.Lock()
	entry.Status = "done"
	entry.DoneAt = time.Now().UTC()
	storeMu.Unlock()

	// Persist final state to MongoDB.
	go mongoSaveMetadata(entry)

	log.Printf("[%s] ✅ done '%s'", workerID, entry.Filename)
}

// ─── CORS middleware ────────────────────────────────────────────────────────────

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Host-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
