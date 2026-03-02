import { useState, useCallback, useEffect, useRef } from 'react'
import './App.css'

const API_BASE = import.meta.env.VITE_API_URL || window.location.origin

const STATUS_LABEL = {
  uploading:  { text: 'Uploading…',   cls: 'status-uploading'  },
  queued:     { text: 'Queued',       cls: 'status-queued'     },
  processing: { text: 'Processing…',  cls: 'status-processing' },
  done:       { text: 'Done ✓',       cls: 'status-done'       },
  duplicate:  { text: 'Duplicate ⚠',  cls: 'status-duplicate'  },
  error:      { text: 'Error ✗',      cls: 'status-error'      },
}

function formatBytes(bytes) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1048576).toFixed(1) + ' MB'
}

function formatDate(dateStr) {
  if (!dateStr) return ''
  const d = new Date(dateStr)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleString(undefined, {
    month: 'short', day: 'numeric', year: 'numeric',
    hour: '2-digit', minute: '2-digit',
  })
}

function FileRow({ entry, isHost, onDelete }) {
  const label = STATUS_LABEL[entry.status] ?? { text: entry.status, cls: '' }
  const canDownload = (entry.status === 'done' || entry.status === 'duplicate') && entry.hash && !entry.hash.startsWith('tmp-')
  const canDelete = isHost && entry.hash && !entry.hash.startsWith('tmp-')
  return (
    <div className="file-row">
      <div className="file-info">
        <span className="file-name">{entry.name}</span>
        {entry.size && <span className="file-size">{formatBytes(entry.size)}</span>}
        {entry.hash && (
          <span className="file-hash" title={entry.hash}>
            {entry.hash.slice(0, 12)}…
          </span>
        )}
        {entry.uploadedAt && (
          <span className="file-date" title="Upload time">{formatDate(entry.uploadedAt)}</span>
        )}
      </div>
      <div className="file-meta">
        {entry.worker && (
          <span className="worker-badge">{entry.worker}</span>
        )}
        <span className={`status-badge ${label.cls}`}>{label.text}</span>
        {canDownload && (
          <a
            className="download-btn"
            href={`${API_BASE}/download/${entry.hash}`}
            download={entry.name}
            title="Download file"
          >
            ⬇ Download
          </a>
        )}
        {canDelete && (
          <button
            className="delete-btn"
            title="Delete file"
            onClick={() => onDelete(entry.hash)}
          >
            🗑 Delete
          </button>
        )}
      </div>
      {entry.message && <p className="file-message">{entry.message}</p>}
    </div>
  )
}

export default function App() {
  const [entries, setEntries] = useState([])
  const [dragging, setDragging] = useState(false)
  const [health, setHealth] = useState(null)
  // Note: sessionStorage is used for convenience. It is cleared when the tab
  // closes and is not shared across tabs. For higher security, an httpOnly
  // cookie set by the server would be preferable.
  const [isHost, setIsHost] = useState(() => !!sessionStorage.getItem('hostToken'))
  const [hostInput, setHostInput] = useState('')
  const [showHostLogin, setShowHostLogin] = useState(false)
  const [hostError, setHostError] = useState('')
  const pollers = useRef({})

  // Poll backend health on mount.
  useEffect(() => {
    fetch(`${API_BASE}/health`)
      .then(r => r.json())
      .then(setHealth)
      .catch(() => setHealth(null))
  }, [])

  // Load previously uploaded files on mount so they appear with download buttons.
  useEffect(() => {
    fetch(`${API_BASE}/files`)
      .then(r => r.json())
      .then(files => {
        if (!Array.isArray(files) || files.length === 0) return
        setEntries(files.map(f => ({
          hash:       f.hash,
          name:       f.filename,
          size:       f.size,
          status:     f.status,
          worker:     f.worker,
          uploadedAt: f.uploaded_at || null,
        })))
      })
      .catch(() => {})
  }, [])

  const updateEntry = useCallback((hash, patch) => {
    setEntries(prev => prev.map(e => e.hash === hash ? { ...e, ...patch } : e))
  }, [])

  const pollStatus = useCallback((hash) => {
    if (pollers.current[hash]) return
    const id = setInterval(async () => {
      try {
        const res = await fetch(`${API_BASE}/status/${hash}`)
        if (!res.ok) {
          // Stop polling if the server can't find this hash (404, etc.).
          clearInterval(id)
          delete pollers.current[hash]
          return
        }
        const data = await res.json()
        updateEntry(hash, {
          status:     data.status,
          worker:     data.worker,
          size:       data.size,
          startedAt:  data.started_at,
          doneAt:     data.done_at,
          uploadedAt: data.uploaded_at || null,
        })
        if (data.status === 'done' || data.status === 'error') {
          clearInterval(id)
          delete pollers.current[hash]
        }
      } catch {
        clearInterval(id)
        delete pollers.current[hash]
      }
    }, 1000)
    pollers.current[hash] = id
  }, [updateEntry])

  const uploadFile = useCallback(async (file) => {
    const tempId = `tmp-${Date.now()}-${file.name}`
    const newEntry = { hash: tempId, name: file.name, status: 'uploading', size: file.size }
    setEntries(prev => [newEntry, ...prev])

    try {
      const body = new FormData()
      body.append('file', file)
      const res = await fetch(`${API_BASE}/upload`, { method: 'POST', body })

      let data
      try {
        data = await res.json()
      } catch {
        const ct = res.headers.get('content-type') || 'unknown'
        throw new Error(res.ok
          ? `Server returned an unexpected response (HTTP ${res.status}, ${ct})`
          : `Upload failed (HTTP ${res.status})`)
      }

      if (!res.ok) {
        throw new Error(data.error || data.message || `Upload failed (HTTP ${res.status})`)
      }

      // Replace temp entry with real hash entry.
      // If the real hash is already in the list (e.g. a previously uploaded
      // file), remove the temp row and update the existing entry when it's a duplicate.
      setEntries(prev => {
        const alreadyExists = prev.some(e => e.hash === data.hash && e.hash !== tempId)
        if (alreadyExists) {
          const filtered = prev.filter(e => e.hash !== tempId)
          if (data.status === 'duplicate') {
            return filtered.map(e =>
              e.hash === data.hash
                ? { ...e, status: 'duplicate', message: data.message }
                : e
            )
          }
          return filtered
        }
        return prev.map(e =>
          e.hash === tempId
            ? { ...e, hash: data.hash, status: data.status, message: data.message, uploadedAt: data.uploaded_at || new Date().toISOString() }
            : e
        )
      })

      if (data.status !== 'duplicate' && data.status !== 'error') {
        pollStatus(data.hash)
      }
    } catch (err) {
      setEntries(prev => prev.map(e =>
        e.hash === tempId ? { ...e, status: 'error', message: err.message } : e
      ))
    }
  }, [pollStatus])

  const handleFiles = useCallback((files) => {
    Array.from(files).forEach(uploadFile)
  }, [uploadFile])

  const handleHostLogin = useCallback(() => {
    if (!hostInput.trim()) return
    sessionStorage.setItem('hostToken', hostInput.trim())
    setIsHost(true)
    setShowHostLogin(false)
    setHostInput('')
    setHostError('')
  }, [hostInput])

  const handleHostLogout = useCallback(() => {
    sessionStorage.removeItem('hostToken')
    setIsHost(false)
    setShowHostLogin(false)
  }, [])

  const handleDeleteFile = useCallback(async (hash) => {
    const token = sessionStorage.getItem('hostToken')
    if (!token) return
    try {
      const res = await fetch(`${API_BASE}/delete/${hash}`, {
        method: 'DELETE',
        headers: { 'X-Host-Token': token },
      })
      if (res.status === 403) {
        setHostError('Invalid host token. Please log in again.')
        sessionStorage.removeItem('hostToken')
        setIsHost(false)
        return
      }
      if (!res.ok) {
        const data = await res.json().catch(() => null)
        setHostError(data?.error || `Delete failed (HTTP ${res.status})`)
        return
      }
      setEntries(prev => prev.filter(e => e.hash !== hash))
    } catch (err) {
      setHostError(err.message)
    }
  }, [])

  const onDrop = useCallback((e) => {
    e.preventDefault()
    setDragging(false)
    handleFiles(e.dataTransfer.files)
  }, [handleFiles])

  const onDragOver = useCallback((e) => { e.preventDefault(); setDragging(true) }, [])
  const onDragLeave = useCallback(() => setDragging(false), [])

  return (
    <div className="app">
      <header className="app-header">
        <h1>📂 Distributed File Upload</h1>
        <p className="subtitle">
          SHA-256 deduplication · etcd distributed locking · multi-worker processing
        </p>
        {health && (
          <div className="health-bar">
            <span className={`dot ${health.etcd ? 'green' : 'yellow'}`} />
            etcd: {health.etcd ? 'connected' : 'standalone'}
            &nbsp;|&nbsp;
            <span className={`dot ${health.mongo ? 'green' : 'yellow'}`} />
            mongo: {health.mongo ? 'connected' : 'standalone'}
            &nbsp;|&nbsp;
            <span className="dot green" />
            workers: {health.workers}
          </div>
        )}
        <div className="host-section">
          {isHost ? (
            <div className="host-status">
              <span className="host-badge">🔑 Host Mode</span>
              <button className="host-logout-btn" onClick={handleHostLogout}>Logout</button>
            </div>
          ) : (
            <button className="host-login-toggle" onClick={() => setShowHostLogin(v => !v)}>
              🔑 Host Login
            </button>
          )}
          {showHostLogin && !isHost && (
            <div className="host-login-form">
              <input
                className="host-token-input"
                type="password"
                placeholder="Enter host token…"
                value={hostInput}
                onChange={e => setHostInput(e.target.value)}
                onKeyDown={e => e.key === 'Enter' && handleHostLogin()}
              />
              <button className="host-login-btn" onClick={handleHostLogin}>Login</button>
            </div>
          )}
          {hostError && <p className="host-error">{hostError}</p>}
        </div>
      </header>

      <main>
        <div
          className={`drop-zone ${dragging ? 'dragging' : ''}`}
          onDrop={onDrop}
          onDragOver={onDragOver}
          onDragLeave={onDragLeave}
          onClick={() => document.getElementById('file-input').click()}
        >
          <div className="drop-icon">⬆</div>
          <p>Drag & drop files here, or <strong>click to browse</strong></p>
          <p className="hint">Upload the same file twice to see duplicate detection in action</p>
          <input
            id="file-input"
            type="file"
            multiple
            hidden
            onChange={e => handleFiles(e.target.files)}
          />
        </div>

        {entries.length > 0 && (
          <section className="file-list">
            <h2>Files</h2>
            {[...entries]
              .sort((a, b) => {
                const aTemp = a.hash && a.hash.startsWith('tmp-')
                const bTemp = b.hash && b.hash.startsWith('tmp-')
                if (aTemp && !bTemp) return -1
                if (!aTemp && bTemp) return 1
                const ta = a.uploadedAt ? new Date(a.uploadedAt).getTime() : 0
                const tb = b.uploadedAt ? new Date(b.uploadedAt).getTime() : 0
                return tb - ta
              })
              .map(e => <FileRow key={e.hash} entry={e} isHost={isHost} onDelete={handleDeleteFile} />)}
          </section>
        )}
      </main>
    </div>
  )
}

