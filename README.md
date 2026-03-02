# Distributed File Upload Processing System

A full-stack demonstration of **distributed coordination** and **cloud-native storage** using:

- **Go** backend — HTTP API, SHA-256 deduplication, multi-worker goroutines
- **etcd** — distributed locking so only one worker processes each unique file
- **MongoDB / MongoDB Atlas** — persistent file metadata and binary storage (GridFS)
- **React** frontend — drag-and-drop upload UI with live status polling and file downloads

---

## Table of Contents

1. [System Architecture](#system-architecture)
2. [Features](#features)
3. [Quick Start](#quick-start)
4. [Connecting to MongoDB Atlas](#connecting-to-mongodb-atlas)
5. [Project Structure](#project-structure)
6. [API Reference](#api-reference)
7. [Configuration](#configuration)
8. [Host Mode (File Deletion)](#host-mode-file-deletion)
9. [How It Works](#how-it-works)
10. [Key Concepts](#key-concepts)

---

## System Architecture

```
User (Browser)
      |
      v
React Frontend  (embedded SPA, port 8080)
      |  POST   /upload
      |  GET    /status/:hash
      |  GET    /download/:hash
      |  DELETE /delete/:hash   (host only — requires X-Host-Token header)
      v
Go Backend API  (port 8080)
      |
      +-- SHA-256 Hash --> Deduplication Store (in-memory)
      |
      +-- File Metadata --> MongoDB "files" collection
      |
      +-- File Content  --> MongoDB GridFS  <-- /download/:hash
      |
      +-- Disk Fallback --> ./uploads/  (if MongoDB unavailable)
      |
      +-- Work Queue  --> Worker-1 -+
                          Worker-2 -+--> etcd /locks/<hash>
                          Worker-3 -+
                               |
                          etcd Server (port 2379)
```

---

## Features

| Feature | Description |
|---------|-------------|
| Drag-and-drop upload | Upload any file from the browser |
| SHA-256 deduplication | Same content uploaded twice gives instant `duplicate` response |
| Distributed locking | etcd ensures exactly one worker processes each file across a cluster |
| MongoDB persistence | File metadata and binaries stored in MongoDB / Atlas across restarts |
| GridFS binary storage | Large files stored in MongoDB GridFS |
| Download button | Download any completed file directly from the browser |
| Host-only file deletion | Authenticated hosts can delete any file via a 🗑 Delete button |
| Graceful degradation | Works fully without etcd or MongoDB (standalone / disk-only mode) |
| Docker-first | One command to start everything |

---

## Quick Start

### Prerequisites

| Tool | Version |
|------|---------|
| Docker & Docker Compose | any recent version |

### 1. Clone the repo

```bash
git clone https://github.com/punithsai18/dsCaseStudy.git
cd dsCaseStudy
```

### 2. (Optional) Point to MongoDB Atlas

Skip this step to use the bundled local MongoDB container.

Create a `.env` file in the project root:

```env
MONGO_URI=mongodb+srv://<username>:<password>@<cluster>.mongodb.net/?retryWrites=true&w=majority
MONGO_DB=dscasestudy
```

See [Connecting to MongoDB Atlas](#connecting-to-mongodb-atlas) for the full setup guide.

### 3. Start everything

```bash
docker compose up --build
```

Expected output:

```
dscasestudy-app  | Connected to MongoDB (db: dscasestudy)
dscasestudy-app  | Connected to etcd at etcd:2379
dscasestudy-app  | [worker-1] started
dscasestudy-app  | [worker-2] started
dscasestudy-app  | [worker-3] started
dscasestudy-app  | Server listening on http://localhost:8080
```

### 4. Open the app

Visit **http://localhost:8080** in your browser.

**From another device on the same network**, find your IP:

```bash
hostname -I | awk '{print $1}'   # Linux / macOS
ipconfig                          # Windows
```

Then open **http://\<your-IP\>:8080**.

### 5. Try it out

1. **Drag a file** onto the drop zone — watch it move through `queued -> processing -> done`
2. **Click Download** once it is done to download the file from MongoDB GridFS
3. **Upload the same file again** — see the instant `duplicate` response
4. Upload multiple files simultaneously — observe workers processing them in parallel

---

## Connecting to MongoDB Atlas

[MongoDB Atlas](https://www.mongodb.com/atlas) is MongoDB's fully-managed cloud database. The backend accepts any valid connection string via the `MONGO_URI` environment variable — including Atlas SRV URIs.

### Step 1 — Create a free Atlas cluster

1. Sign up at [mongodb.com/atlas](https://www.mongodb.com/atlas/database) (free tier available)
2. Create a **free M0 cluster** (512 MB, 3-node replica set)
3. In **Database Access**, create a database user with `readWrite` permission
4. In **Network Access**, add your IP address (or `0.0.0.0/0` for access from any IP)
5. Click **Connect > Drivers** and copy the connection string. It looks like:

```
mongodb+srv://<username>:<password>@cluster0.abc12.mongodb.net/?retryWrites=true&w=majority
```

### Step 2 — Configure the app

**Option A — `.env` file (recommended)**

Create a `.env` file in the project root (next to `docker-compose.yml`):

```env
MONGO_URI=mongodb+srv://<username>:<password>@cluster0.abc12.mongodb.net/?retryWrites=true&w=majority
MONGO_DB=dscasestudy
```

Docker Compose automatically reads this file.

**Option B — pass on the command line**

```bash
MONGO_URI="mongodb+srv://..." docker compose up --build
```

**Option C — edit `docker-compose.yml` directly**

```yaml
services:
  app:
    environment:
      MONGO_URI: "mongodb+srv://<user>:<pass>@<cluster>.mongodb.net/?retryWrites=true&w=majority"
      MONGO_DB: dscasestudy
```

### Step 3 — Remove the local `mongo` service (optional)

When using Atlas you do not need the bundled `mongo:7` container. Edit `docker-compose.yml`:

1. Delete the `mongo:` service block
2. Remove `mongo:` from `app.depends_on`
3. Delete the `mongo_data:` volume entry

Then restart:

```bash
docker compose down && docker compose up --build
```

### Step 4 — Verify the connection

```bash
curl http://localhost:8080/health
```

```json
{
  "etcd":      true,
  "mongo":     true,
  "status":    "ok",
  "timestamp": "2025-01-15T10:23:44Z",
  "workers":   3
}
```

`"mongo": true` confirms the backend is connected to your Atlas cluster.

### What gets stored in Atlas

| Collection / Bucket | Contents |
|---------------------|----------|
| `dscasestudy.files` | File metadata (hash, filename, size, status, worker, timestamps) |
| `dscasestudy.fs.files` | GridFS file descriptors |
| `dscasestudy.fs.chunks` | GridFS binary chunks (actual file data) |

---

## Project Structure

```
dsCaseStudy/
+-- main.go              # Go backend: API server, workers, MongoDB integration
+-- go.mod               # Go module dependencies
+-- go.sum               # Dependency checksums
+-- vendor/              # Vendored dependencies (no internet required to build)
+-- docker-compose.yml   # Runs etcd, mongo (local), and the app
+-- Dockerfile           # Multi-stage build (embeds React into Go binary)
+-- uploads/             # Created at runtime (disk fallback for uploaded files)
|
+-- frontend/            # React application
    +-- index.html
    +-- package.json
    +-- src/
        +-- main.jsx
        +-- App.jsx      # Upload UI, status polling, download button
        +-- App.css      # Dark-theme styles
```

---

## API Reference

### `POST /upload`

Upload a file for processing.

**Request:** `multipart/form-data` with field `file`

**Response:**

```json
{
  "hash":    "a3f5d9e87b12c4f0...",
  "status":  "queued",
  "message": "File queued for processing"
}
```

Status values: `queued` | `duplicate`

---

### `GET /status/:hash`

Poll processing status for a file.

**Response:**

```json
{
  "hash":       "a3f5d9e87b12c4f0...",
  "filename":   "report.pdf",
  "size":       204800,
  "status":     "done",
  "worker":     "worker-2",
  "started_at": "2025-01-15T10:23:44Z",
  "done_at":    "2025-01-15T10:23:47Z"
}
```

Status values: `queued` | `processing` | `done` | `duplicate` | `error`

---

### `GET /files`

List all uploaded files and their statuses.

**Response:** JSON array of FileStatus objects (same shape as `/status/:hash`).

---

### `GET /download/:hash`

Download a file by its SHA-256 hash.

- Streams from **MongoDB GridFS** when available
- Falls back to the local **`./uploads/`** directory otherwise
- Sets `Content-Disposition: attachment` so the browser downloads the file

---

### `DELETE /delete/:hash`

Delete a file by its SHA-256 hash. **Requires host authentication.**

**Request headers:**

| Header | Value |
|--------|-------|
| `X-Host-Token` | The value configured in the `HOST_TOKEN` environment variable |

**Response (200 — success):**

```json
{
  "status": "deleted",
  "hash":   "a3f5d9e87b12c4f0..."
}
```

**Error responses:**

| Status | Meaning |
|--------|---------|
| `403 Forbidden` | Token missing, wrong, or `HOST_TOKEN` not set on the server |
| `404 Not Found` | No file with that hash exists |

On success the file is removed from: the in-memory store, disk (`./uploads/`), MongoDB GridFS, and the `files` metadata collection.

---

### `GET /health`

Backend health check.

**Response:**

```json
{
  "status":    "ok",
  "etcd":      true,
  "mongo":     true,
  "workers":   3,
  "timestamp": "2025-01-15T10:23:44Z"
}
```

---

## Configuration

All settings are controlled via environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server listen port |
| `ETCD_ENDPOINT` | `localhost:2379` | etcd server address |
| `MONGO_URI` | `mongodb://localhost:27017` | MongoDB connection string. Accepts local URIs and Atlas SRV (`mongodb+srv://...`) |
| `MONGO_DB` | `dscasestudy` | MongoDB database name |
| `HOST_TOKEN` | _(empty)_ | Secret token that enables the `DELETE /delete/:hash` endpoint. When empty, all delete requests are rejected. See [Host Mode (File Deletion)](#host-mode-file-deletion). |
| `VITE_API_URL` | auto-detect | Backend URL used by the React dev server (`frontend/.env.local`) |

### Running without Docker

```bash
# Start etcd only (for distributed locking)
docker compose up etcd -d

# Run the Go server
go run main.go

# With Atlas:
MONGO_URI="mongodb+srv://..." go run main.go
```

For frontend hot-reload:

```bash
cd frontend
npm install
npm run dev   # Vite dev server on :5173
```

Create `frontend/.env.local`:

```env
VITE_API_URL=http://localhost:8080
```

### Port conflicts

```bash
docker compose down          # stop Docker containers
PORT=9090 go run main.go     # Linux / macOS — different port
$env:PORT="9090"; go run main.go   # Windows PowerShell
```

---

## Host Mode (File Deletion)

The application supports a **host mode** that lets a privileged user delete any uploaded file. The feature is disabled by default — it only activates when `HOST_TOKEN` is set on the server.

### Step 1 — Set the host token on the server

**Docker Compose (`.env` file):**

```env
HOST_TOKEN=replace-with-a-strong-secret
```

Docker Compose reads `.env` automatically. Add `HOST_TOKEN` to the `app` service's `environment` block in `docker-compose.yml` if you prefer to set it there:

```yaml
services:
  app:
    environment:
      HOST_TOKEN: "replace-with-a-strong-secret"
```

**Without Docker:**

```bash
HOST_TOKEN=replace-with-a-strong-secret go run main.go
```

### Step 2 — Log in to Host Mode in the browser

1. Open **http://localhost:8080**
2. Click the **🔑 Host Login** button in the top-right area of the header
3. Enter the same token you set in `HOST_TOKEN`
4. Click **Login** (or press Enter)

A **🔑 Host Mode** badge replaces the login button to confirm authentication. The token is stored in `sessionStorage` — it is automatically cleared when the browser tab is closed.

### Step 3 — Delete files

While in Host Mode, every file row shows a **🗑 Delete** button. Click it to permanently delete the file from the server.

If the token is rejected by the server (e.g. it was changed and the page was not reloaded), the UI automatically logs out and displays an error message.

### Security notes

- Choose a long, random token (e.g. `openssl rand -hex 32`).
- The token is compared using a constant-time algorithm on the server to prevent timing attacks.
- `sessionStorage` is accessible to JavaScript on the same page. For higher security, consider serving the application over HTTPS so the token travels only over an encrypted connection.
- When `HOST_TOKEN` is not set, the `DELETE /delete/:hash` endpoint always returns `403 Forbidden`, regardless of what the client sends.

---

## How It Works

### Upload flow

```
Browser --> POST /upload --> Go API
                               |
                          Read bytes, compute SHA-256 hash
                               |
               +-- Already seen? --> return "duplicate"
               |
               +-- New file --> save to disk (sync)
                                save to GridFS (async)
                                upsert metadata to MongoDB (async)
                                enqueue processing job
                                return "queued"
```

### Processing flow (distributed)

```
Worker picks job from queue
         |
         +-- etcd available? --YES--> acquire /locks/<hash>
         |                                |
         |                           Mark "processing"
         |                           Simulate work (3 s)
         |                           Mark "done"
         |                           Upsert metadata to MongoDB
         |                           Release lock
         |
         +-- etcd unavailable? -------> Same steps, no lock
```

### Download flow

```
Browser --> GET /download/<hash> --> Go API
                                       |
                                  Lookup hash in store
                                       |
                    +-- GridFS available? --YES--> stream from GridFS
                    |
                    +-- Fallback --> stream from ./uploads/<hash>_<name>
```

### Startup restore

On restart the backend re-hydrates its in-memory store:

1. Query MongoDB `files` collection — restore all known file records
2. Scan `./uploads/` — add any files not already in the store

All files survive container restarts even without MongoDB (disk-only mode).

---

## Key Concepts

### Content-Addressable Storage

Files are indexed by their SHA-256 hash, not their names. This enables:

- **Deduplication** — identical content is stored once
- **Integrity verification** — hash confirms the file was not corrupted
- **Deterministic locking** — same content always maps to the same lock key

### MongoDB GridFS

GridFS splits files into 255 KB chunks stored in two collections:
- `fs.files` — file metadata (name, size, upload date, custom metadata)
- `fs.chunks` — binary data chunks

Atlas replicates these automatically across nodes in the cluster.

### etcd Distributed Consensus

etcd uses the **Raft consensus algorithm** which guarantees:

- **Linearizability** — every lock acquisition is globally ordered
- **Fault tolerance** — cluster survives minority node failures
- **TTL-based lease expiry** — crashed workers release locks automatically after 30 s

### Graceful Degradation

| Service | If unavailable |
|---------|---------------|
| etcd | Workers still process files; no distributed locking |
| MongoDB | Files saved to `./uploads/` only; downloads served from disk |

The backend always starts regardless of which services are reachable.
