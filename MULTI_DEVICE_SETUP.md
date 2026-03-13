# Connecting Additional Devices to the Same etcd Server

This guide explains how to run the **Go backend on multiple machines** (or containers) that all share one central etcd server. Every backend instance will participate in distributed locking, so only one worker across the whole cluster processes each unique file at a time.

---

## Table of Contents

1. [Overview](#overview)
2. [Network Requirements](#network-requirements)
3. [Step 1 — Expose etcd to the Network](#step-1--expose-etcd-to-the-network)
4. [Step 2 — Configure the Backend on Each Device](#step-2--configure-the-backend-on-each-device)
5. [Step 3 — Start the Backend on Each Device](#step-3--start-the-backend-on-each-device)
6. [Step 4 — Verify Connectivity](#step-4--verify-connectivity)
7. [Step 5 — Run the Frontend](#step-5--run-the-frontend)
8. [How Distributed Locking Works Across Machines](#how-distributed-locking-works-across-machines)
9. [Troubleshooting](#troubleshooting)
10. [Security Notes](#security-notes)

---

## Overview

```
Device A (etcd host)            Device B                Device C
─────────────────────           ─────────────────────   ─────────────────────
etcd server :2379   ◄──────────── Go backend :8080  ◄── Go backend :8080
Go backend  :8080   (LAN/VPN)
React UI    :5173
```

All backend instances connect to the **same etcd endpoint**. etcd serialises lock acquisition so that even when `Device B` and `Device C` pick up the same file hash at the same moment, only one of them will hold the lock and process the file.

---

## Network Requirements

| Requirement | Detail |
|-------------|--------|
| etcd client port | **2379/TCP** must be reachable from every device that runs the backend |
| etcd peer port | **2380/TCP** only needed between etcd nodes in a multi-node etcd cluster |
| Backend API port | **8080/TCP** — each device exposes its own backend; the React UI can point to any of them |
| Protocol | Plain HTTP (default). See [Security Notes](#security-notes) for TLS. |

---

## Step 1 — Expose etcd to the Network

The default `docker-compose.yml` advertises etcd only on `localhost`. Update `ETCD_ADVERTISE_CLIENT_URLS` so remote devices can connect.

### Find the host machine's LAN IP

```bash
# Linux / macOS
ip route get 1 | awk '{print $7; exit}'
# or
hostname -I | awk '{print $1}'
```

Example result: `10.84.79.147`

### Update `docker-compose.yml`

Replace `localhost` in `ETCD_ADVERTISE_CLIENT_URLS` with the host machine's LAN IP:

```yaml
services:
  etcd:
    image: quay.io/coreos/etcd:v3.5.17
    container_name: etcd
    ports:
      - "2379:2379"
      - "2380:2380"
    environment:
      ETCD_NAME: etcd0
      ETCD_INITIAL_CLUSTER: etcd0=http://etcd:2380
      ETCD_INITIAL_CLUSTER_TOKEN: etcd-cluster
      ETCD_INITIAL_CLUSTER_STATE: new
      ETCD_LISTEN_CLIENT_URLS: http://0.0.0.0:2379
      ETCD_ADVERTISE_CLIENT_URLS: http://10.84.79.147:2379   # ← host LAN IP
      ETCD_LISTEN_PEER_URLS: http://0.0.0.0:2380
      ETCD_INITIAL_ADVERTISE_PEER_URLS: http://etcd:2380
    healthcheck:
      test: ["CMD", "etcdctl", "endpoint", "health"]
      interval: 10s
      timeout: 5s
      retries: 5
```

Restart etcd after saving:

```bash
docker compose down && docker compose up -d
```

### Open the firewall (if applicable)

```bash
# Ubuntu / Debian (ufw)
sudo ufw allow 2379/tcp

# CentOS / RHEL (firewalld)
sudo firewall-cmd --permanent --add-port=2379/tcp
sudo firewall-cmd --reload
```

---

## Step 2 — Configure the Backend on Each Device

The etcd endpoint is hardcoded in `main.go` as `localhost:2379`. On every **remote** device, replace that value with the host machine's IP before running:

```go
// main.go  (lines ~60-80)
etcdClient, err = clientv3.New(clientv3.Config{
    Endpoints:   []string{"10.84.79.147:2379"},   // ← host LAN IP
    DialTimeout: 5 * time.Second,
    Logger:      zap.NewNop(),
})
```

> **Tip — use an environment variable instead of editing the source**
>
> Replace the hardcoded string with an environment variable so you can configure each machine without modifying code:
>
> ```go
> etcdEndpoint := os.Getenv("ETCD_ENDPOINT")
> if etcdEndpoint == "" {
>     etcdEndpoint = "localhost:2379"
> }
> etcdClient, err = clientv3.New(clientv3.Config{
>     Endpoints:   []string{etcdEndpoint},
>     DialTimeout: 5 * time.Second,
>     Logger:      zap.NewNop(),
> })
> ```
>
> Then start the backend on each remote device with:
>
> ```bash
> ETCD_ENDPOINT=10.84.79.147:2379 go run main.go
> ```

---

## Step 3 — Start the Backend on Each Device

**Device A (etcd host — already running etcd):**

```bash
# etcd is already up from docker compose
go run main.go
# Uses localhost:2379 — no change needed
```

**Device B and Device C (remote devices):**

```bash
# Clone or copy the project
git clone https://github.com/punithsai18/dsCaseStudy.git
cd dsCaseStudy

# Point the backend to Device A's etcd
ETCD_ENDPOINT=10.84.79.147:2379 go run main.go
```

Expected output on each device:

```
✔  Connected to etcd at 10.84.79.147:2379
[worker-1] started
[worker-2] started
[worker-3] started
🚀  Backend listening on http://localhost:8080
```

---

## Step 4 — Verify Connectivity

From any remote device, confirm it can reach etcd:

```bash
# Using curl
curl http://10.84.79.147:2379/health

# Using etcdctl (if installed)
etcdctl --endpoints=http://10.84.79.147:2379 endpoint health
```

Expected response:

```json
{"health":"true","reason":""}
```

Check that the backend on the remote device is connected:

```bash
curl http://localhost:8080/health
```

```json
{
  "etcd":      true,
  "status":    "ok",
  "timestamp": "2025-01-15T10:23:44Z",
  "workers":   3
}
```

`"etcd": true` confirms the instance holds an active etcd connection.

---

## Step 5 — Run the Frontend

The React frontend can point to **any backend instance**. Set the API base URL to whichever device you want the UI to talk to.

In `frontend/src/App.jsx` (or via a `.env` file), update the API base URL:

```js
// .env.local  (create this file in the frontend/ directory)
VITE_API_URL=http://192.168.1.11:8080   // Device B's backend
```

Then start the frontend:

```bash
cd frontend
npm install
npm run dev
```

Open **http://localhost:5173** in your browser. Uploads will be handled by Device B's backend, but the distributed lock is shared with every other backend in the cluster.

---

## How Distributed Locking Works Across Machines

```
Device B — worker-1                Device C — worker-2
uploads file X                     uploads file X (same content)
SHA-256 hash = abc123              SHA-256 hash = abc123
         │                                  │
         ▼                                  ▼
etcd: PUT /locks/abc123   ◄────────────────► etcd: PUT /locks/abc123
         │                                  │
  Lock acquired ✔                    Waiting… (blocked by Device B)
         │                                  │
  Processing file X                         │
         │                                  │
  Unlock /locks/abc123 ──────────────────► Lock acquired ✔
                                            │
                                     Processes (or detects duplicate)
```

Because all backends share the **same etcd server**, the lock namespace is global. It does not matter which physical machine submits the file — etcd enforces that only one worker across the entire cluster holds the lock at any time.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `⚠ etcd unavailable` on remote device | Port 2379 not reachable | Check firewall; confirm Docker port binding |
| `connection refused` on curl to etcd | Wrong IP or etcd not started | Verify IP with `hostname -I`; run `docker compose ps` |
| `etcd: true` but locks not shared | Different `ETCD_ENDPOINT` on each device | Ensure all devices point to the same IP |
| Backend on Device A works, B does not | `ETCD_ADVERTISE_CLIENT_URLS` still set to `localhost` | Update docker-compose and restart |
| Lock acquisition timeout | Network latency or etcd overloaded | Increase `DialTimeout` and lock context timeout in `main.go` |

---

## Security Notes

> The default setup uses **plain HTTP** and no authentication. This is fine for a local network demo but **not** suitable for production.

For production deployments:

1. **Enable TLS** — generate certificates and set `ETCD_CERT_FILE`, `ETCD_KEY_FILE`, `ETCD_TRUSTED_CA_FILE` in the etcd environment; update `clientv3.Config` to use `tls.Config`.
2. **Enable etcd authentication** — create users/roles with `etcdctl user add` and `etcdctl auth enable`.
3. **Use a VPN or private subnet** — never expose port 2379 to the public internet without TLS and authentication.
4. **Restrict CORS** — in `main.go`, replace `"*"` in the CORS middleware with the exact frontend origin(s).
