# Algorithms Used in Distributed File Upload Processing System

This document outlines the core algorithms and distributed systems primitives used to ensure reliability, consistency, and efficiency across the system.

---

## Algorithm 1 — Raft Consensus (via etcd)

**What it does:**  
Raft is a distributed consensus algorithm that keeps a cluster of nodes in
agreement about a shared log of state changes.

**How it is used in this project:**  
etcd internally runs Raft to replicate every key-value write (including lock
acquisitions/releases at `/locks/<hash>`) across all etcd nodes. Before the
Go workers can acquire a lock, the Raft leader must collect acknowledgements
from a majority of etcd nodes.

```
Worker calls mu.Lock()
       │
       ▼
etcd client ──► Raft leader (etcd-0)
                     │  Proposes log entry: "grant lock /locks/<hash>"
                     ▼
               ┌─────────────────────────────────────┐
               │  etcd-1  ACK  │  etcd-2  ACK        │  Quorum (2/3) reached
               └─────────────────────────────────────┘
                     │  Commit log entry
                     ▼
               Lock granted to Worker ✔
               (All other workers blocked)
```

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Leader Election | If etcd-0 crashes, etcd-1 or etcd-2 becomes the new leader automatically |
| Linearizability | No two workers hold the same lock simultaneously, globally |
| Fault Tolerance | 3-node cluster tolerates 1 failure; 5-node cluster tolerates 2 |
| TTL Lease Expiry | If a worker crashes while holding a lock, the 30 s session TTL causes automatic release |

**Why Raft was chosen:**  
etcd was chosen because it is the industry-standard coordination store (used by
Kubernetes). Raft is simpler to understand than Paxos and provides strong
consistency guarantees needed for mutual exclusion.

---

## Algorithm 2 — Distributed Locking (etcd Mutex)

**What it does:**  
Implements mutual exclusion across processes on different machines using etcd
sessions and leases.

**How it is used in this project:**  
Each worker creates an etcd session (with a 30 s TTL) and then calls
`concurrency.NewMutex(session, "/locks/"+hash).Lock(ctx)`. Only one worker
across the entire cluster can hold the lock for a given hash at a time.

```
Worker-1 (Machine A)                   Worker-2 (Machine B)
POST /upload same file                  POST /upload same file
hash = abc123                           hash = abc123
        │                                       │
        ▼                                       ▼
etcd: TryAcquire /locks/abc123      etcd: TryAcquire /locks/abc123
        │                                       │
   Lock acquired ✔                        Blocked (waiting)
        │                                       │
   Mark status = "processing"                   │
   Simulate / process work (3 s)                │
   Mark status = "done"                         │
   Save to MongoDB                              │
   Release lock ──────────────────────────► Lock acquired ✔
                                                 │
                                          Detect duplicate → skip
```

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Mutual Exclusion | Exactly one worker processes a specific hash at any time |
| Deadlock Freedom | Locks automatically expire if a worker node crashes (via TTL) |
| Global Visibility | Locking state is visible across all machines connecting to etcd |

**Why it was chosen:**  
Distributed locking is essential for "Exactly-Once" processing semantics in a multi-machine environment, preventing race conditions and redundant computation.

---

## Algorithm 3 — SHA-256 Content-Addressable Deduplication

**What it does:**  
Computes a cryptographic hash of uploaded file content. The hash becomes the
file's unique key in every store (in-memory map, MongoDB, GridFS, disk).

**How it is used in this project:**

```
Browser uploads file bytes
          │
          ▼
Go API reads all bytes → crypto/sha256.Sum256(bytes) → hex string (64 chars)
          │
          ├── Already in fileStore (map)? → return status "duplicate" immediately
          │
          └── New hash → save file under hash as key
                         enqueue for processing
                         use hash as etcd lock key
                         use hash as MongoDB document key
                         use hash as GridFS filename
```

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Determinism | The same content always produces the same hash |
| Collision Resistance | Practically impossible for two different files to have the same hash |
| Integrity | The hash can be re-verified on download to ensure data wasn't corrupted |

**Why it was chosen:**  
SHA-256 provides a robust way to implement deduplication. By indexing files by content rather than name, we save storage space and avoid redundant processing of identical files.

---

## Algorithm 4 — Leader Election (via etcd)

**What it does:**  
Elects one worker as the "Cluster Leader" to coordinate activities or provide a single status view.

**How it is used in this project:**  
Workers use the etcd Election API to "campaign." The first worker to successfully create an ephemeral key in etcd becomes the leader.

```
Worker-1 ──┐
Worker-2 ──┼──► etcd /election/coordinator  ← one candidate wins
Worker-3 ──┘         │
                     │  elected leader ID stored in currentLeader
                     │  exposed via GET /health { "leader": "worker-1" }
```

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Single Leader | At most one node is elected leader at any given time |
| Automatic Failover | If the leader crashes, another worker is elected automatically |
| Liveness | As long as etcd and at least one worker are up, a leader will exist |

**Why it was chosen:**  
Leader election demonstrates how distributed coordination can be used for administrative tasks, such as designating which node is responsible for specific management activities.

---

## Algorithm 5 — GridFS Chunking Algorithm

**What it does:**  
Splits large files into smaller chunks (default 255 KB) to store them in MongoDB collections.

**How it is used in this project:**  
The Go backend uses the MongoDB GridFS driver to stream file bytes. GridFS handles the logic of breaking the file into chunks (`fs.chunks`) and managing metadata (`fs.files`).

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Scalability | Can store files larger than the 16MB MongoDB document limit |
| Random Access | Allows reading specific portions of a file without loading the whole thing |
| Replication | Chunks are automatically replicated across the MongoDB Atlas cluster |

**Why it was chosen:**  
GridFS is the standard way to store binary assets in MongoDB, ensuring that file storage inherits the database's existing high availability and scaling features.

---

## Algorithm 6 — Constant-Time Comparison (Security)

**What it does:**  
Compares two strings in a way that always takes the same amount of time, regardless of how many characters match.

**How it is used in this project:**  
Used in `handleDeleteFile` to validate the `X-Host-Token` against the configured `HOST_TOKEN`.

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Side-Channel Resistance | Prevents attackers from guessing tokens character-by-character via timing |
| Privacy | matching chars don't leak performance clues |

**Why it was chosen:**  
Security best practice for any token-based authentication to prevent timing attacks.

---

## Algorithm 7 — Worker Pool Pattern

**What it does:**  
Uses a fixed number of worker goroutines to process jobs from a shared channel.

**How it is used in this project:**  
A buffered channel `workQueue` acts as the job distributor. Three goroutines (`worker-1`, `worker-2`, `worker-3`) continuously pull tasks from this channel.

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Throttling | Prevents the system from being overwhelmed by too many concurrent tasks |
| Resource Efficiency | Reuses a small number of goroutines instead of spawning high numbers |
| Load Balancing | Jobs are naturally distributed to the first available worker |

**Why it was chosen:**  
Standard Go concurrency pattern that balances responsiveness with resource management.

---

## Algorithm 8 — Status Polling (Frontend)

**What it does:**  
Periodically checks the server for status updates on a specific file using its hash.

**How it is used in this project:**  
The React frontend uses `setInterval` to call `GET /status/:hash` every 1 second until the file reaches a terminal state (`done`, `duplicate`, or `error`).

**Key guarantees:**

| Guarantee | Meaning |
|---|---|
| Consistency | The UI eventually reflects the true state of the backend |
| Resource Conservation | Polling stops immediately once a final state is reached |
| Independence | Each file upload is polled independently |

**Why it was chosen:**  
A simple and robust way to provide real-time updates without the complexity of managing persistent WebSocket connections for short-lived tasks.
