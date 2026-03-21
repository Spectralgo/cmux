# Review R2: Auth + Request/Response Flow (End-to-End Trace)

> Reviewer: cmux/crew/tester
> Date: 2026-03-21
> Scope: Design spec (WS-TRANSPORT-DESIGN.md) traced against actual RN client code
> Status: **Go WS files are DESIGN ONLY — not yet implemented**. This review validates
> the design spec against the live RN client (`ws-client.ts`, `useConnection.ts`,
> `protocol.ts`, `store.ts`) and existing Go types (`main.go` rpcResponse struct).

---

## FLOW 1: AUTH

**Trace:**

| Step | Actor | Detail | Verdict |
|------|-------|--------|---------|
| 1. RN sends auth | `ws-client.ts:82-86` | `{"method":"auth","params":{"token":"abc"}}` — NO `id` field. `RpcRequest.id` is `undefined`, so `JSON.stringify` omits it. | ✅ PASS |
| 2. Go parses auth | design: `authenticate()` | `json.Unmarshal` into `rpcRequest{ID any}` — missing field → `ID` is `nil` | ✅ PASS |
| 3. Go responds | design: `rpcResponse{OK: true, Result: {authenticated: true}}` | `ID` is `nil` + `json:"id,omitempty"` → field omitted from JSON | ✅ PASS |
| 4. Wire bytes | — | `{"ok":true,"result":{"authenticated":true}}` — confirmed via Go test (`go run test_omitempty.go`) | ✅ PASS |
| 5. RN checks auth | `ws-client.ts:98` | `isResponse(msg)`: `'ok' in msg` → `true`. `!msg.id`: `msg.id` is `undefined`, `!undefined` is `true`. `msg.ok`: `true`. `result.authenticated`: `true`. | ✅ PASS |
| 6. RN signals connected | `ws-client.ts:99-101` | `attempts = 0`, `onConnectionChange(true)` → triggers `system.tree` fetch | ✅ PASS |

**VERIFY: Does Go's rpcResponse with nil ID actually produce JSON without "id" field?**

```
$ go run test_omitempty.go
Auth response (nil ID): {"ok":true,"result":{"authenticated":true}}
Auth resp from parsed req: {"ok":true,"result":{"authenticated":true}}
```

✅ **Confirmed.** `omitempty` on `any` type drops the field when value is `nil`.

**Auth failure path:** Design sends `{ok: false, error: {code: "auth_failed"}}`.
RN's auth check (`!msg.id && msg.ok && result.authenticated`) fails on `msg.ok === false`.
Message falls through all handlers and reaches `this.onMessage(msg)` → `handleMessage` →
`isEvent(msg)` returns false (no `event` field) → silently dropped. Go then closes the
connection → `ws.onclose` fires → `onConnectionChange(false)` + reconnect.

⚠️ **WARNING: Silent auth failure.** The RN client has no explicit handling for auth
rejection. It relies on the connection closing to trigger reconnect with exponential
backoff. This means a bad token causes an infinite reconnect loop with no user-visible
error. Not a protocol mismatch, but a UX gap.

---

## FLOW 2: SYSTEM.TREE REQUEST

**Trace:**

| Step | Actor | Detail | Verdict |
|------|-------|--------|---------|
| 1. RN sends | `ws-client.ts:57-60` | `{"id":"1","method":"system.tree"}` — `id` is `String(this.nextId++)`, always a string | ✅ PASS |
| 2. Go parses | design: `dispatch()` | `req.ID` = `"1"` (string, confirmed: Go unmarshals JSON string `"1"` as Go string `"1"`) | ✅ PASS |
| 3. Go routes | design: `handleProxiedRequest` | `case "system.tree"` → `cmuxTopology(cmuxSocket)` | ✅ PASS |
| 4. cmuxTopology | design: `ws_cmux_proxy.go` | Calls `cmuxCall("system.tree", {all_windows: true})` → returns `json.RawMessage` | ✅ PASS |
| 5. Go responds | design | `rpcResponse{ID: req.ID, OK: true, Result: <raw tree>}` | See ⚠️ below |
| 6. Wire bytes | — | `{"id":"1","ok":true,"result":{...TreeResult...}}` | ✅ PASS |
| 7. RN routes | `ws-client.ts:105-108` | `isResponse(msg)` → true. `msg.id` → truthy. `pendingRequests.get(String(msg.id))` → `String("1")` = `"1"` → matches key | ✅ PASS |
| 8. RN unpacks | `useConnection.ts:41-44` | `resp.result as TreeResult` → `tree.windows?.[0]?.workspaces ?? []` | ✅ PASS |

**VERIFY: Does handleProxiedRequest set resp.ID to req.ID?**

The design spec's pseudocode (WS-TRANSPORT-DESIGN.md §1.2) shows:
```
case "system.tree":
    result, err := cmuxTopology(cmuxSocket)
    // Return raw JSON result (TreeResult shape) directly
```

⚠️ **WARNING: ID propagation not explicitly shown in pseudocode.** The existing
`handleRequest` in `main.go` sets `ID: req.ID` on every response path (30+ occurrences).
The design must follow the same pattern. Implementors: every return path in
`handleProxiedRequest` MUST set `ID: req.ID`. Missing this would cause the RN client to
silently drop responses (no matching pending request).

**VERIFY: Go response ID type matches RN String comparison.**

```
$ go run test_omitempty.go
Parsed tree req.ID: 1 (type: string)    # Go preserves string type
Tree resp from parsed req: {"id":"1",...} # Re-serializes as string
```

RN does `String(msg.id)` → `String("1")` = `"1"`. ✅ Match.

If Go ever sent a numeric ID (e.g., `"id":1`), `String(1)` = `"1"` would still match.
The `String()` wrapper is defensive. ✅ No mismatch.

---

## FLOW 3: SURFACE.SUBSCRIBE + STREAMING

**Trace:**

| Step | Actor | Detail | Verdict |
|------|-------|--------|---------|
| 1. RN sends | `useConnection.ts:50-52` via `ws-client.ts:send()` | `{"id":"2","method":"surface.subscribe","params":{"surface_id":"UUID"}}` | ✅ PASS |
| 2. Go routes | design: `dispatch()` | `case "surface.subscribe"` → `handleSubscribe` | ✅ PASS |
| 3. Go ack | design: `ws_stream.go` | `{id:"2", ok:true, result:{subscribed:"UUID"}}` | ✅ PASS |
| 4. RN ack routing | `ws-client.ts:105-108` | `isResponse(msg)` + `msg.id` → pendingRequests resolves promise | ✅ PASS |
| 5. Go spawns watcher | design | `go watchSurface(ctx, cl, surfaceID)` | ✅ PASS |
| 6. Go sends screen_init | design | `{"event":"screen_init","stream_id":"UUID","data_base64":"<base64>"}` | ✅ PASS |
| 7. RN message routing | `ws-client.ts:89-116` | Auth check → SKIP (no `ok` field). Response check → SKIP (no `ok`). Forward to `onMessage`. | ✅ PASS |
| 8. RN event handling | `useConnection.ts:82-86` | `isEvent(msg)` → true (`'event' in msg`). `case 'screen_init'`. `atob(data_base64)` → `{lines:[...]}`. `store.setScreen(stream_id, data.lines)`. | ✅ PASS |

**VERIFY: The ack response reaches the pending promise (id-based routing).**

The `send()` method in `ws-client.ts:57-58` stores the resolver in `pendingRequests`
keyed by `id`. The ack response has `id: "2"` and `ok: true` → `isResponse` matches →
`pendingRequests.get("2")` resolves the promise. ✅

**VERIFY: The screen_init event is NOT routed to pendingRequests.**

The event `{"event":"screen_init","stream_id":"UUID","data_base64":"..."}` has no `ok`
field → `isResponse(msg)` returns `false` → skips both auth check and pending request
routing → goes to `this.onMessage(msg)`. ✅

**VERIFY: isEvent() vs isResponse() discrimination.**

| Message type | Has `ok`? | Has `event`? | `isResponse()` | `isEvent()` |
|-------------|-----------|-------------|----------------|-------------|
| Auth ack | yes | no | true | false |
| Request response | yes | no | true | false |
| screen_init event | no | yes | false | true |
| screen_diff event | no | yes | false | true |
| topology event | no | yes | false | true |

No overlap between Go's `rpcResponse` and `rpcEvent` struct fields. The type guards
are mutually exclusive in practice. ✅

⚠️ **WARNING: Unhandled promise rejection on subscribe failure.** The subscribe calls
at `useConnection.ts:50-52` are fire-and-forget:
```ts
for (const surf of surfaces) {
  client.send('surface.subscribe', { surface_id: surf.id });
}
```
If a subscribe fails (Go returns error or 10s timeout fires), the rejected promise is
unhandled. This won't break the protocol flow but may trigger React Native's unhandled
rejection warning. Add `.catch()` or `void` annotation.

---

## FLOW 4: SURFACE.INPUT (COMMAND SEND)

**Trace:**

| Step | Actor | Detail | Verdict |
|------|-------|--------|---------|
| 1. RN sends | `ws-client.ts:send()` | `{"id":"3","method":"surface.input","params":{"surface_id":"UUID","text":"ls\n"}}` | ✅ PASS |
| 2. Go routes | design: `dispatch()` | `case "surface.input"` → `handleProxiedRequest` | ✅ PASS |
| 3. Go translates | design: `ws_cmux_proxy.go` | `surface.input` → `cmuxSendText(socket, surfaceID, text)` → calls cmux.sock `surface.send_text` | ✅ PASS |
| 4. Param extraction | design | `surfaceID := params["surface_id"]`, `text := params["text"]` | ✅ PASS |
| 5. cmux.sock call | design: `cmuxSendText` | Sends `surface.send_text` with `{"surface_id": surfaceID, "text": text}` | ✅ PASS |
| 6. Go responds | design | `{id:"3", ok:true, result:{sent:true}}` | ✅ PASS |
| 7. RN resolves | `ws-client.ts:105-109` | pendingRequests matches id "3" | ✅ PASS |

**VERIFY: Method name translation in handleProxiedRequest.**

The design spec explicitly documents (§2.2):
- RN `surface.input` → Go proxy → cmux.sock `surface.send_text`
- RN `surface.keys` → Go proxy → cmux.sock `surface.send_key`

Confirmed in existing CLI: `cli.go:74` maps `send` command to `surface.send_text`.
The proxy does the same translation internally. ✅

**VERIFY: Param names match.**

RN sends `surface_id` + `text`. The Go proxy extracts these same keys and passes to
`cmuxSendText` which forwards them to cmux.sock. The CLI's `flagToParamKey` maps
`--surface` flag to `surface_id` param — same key name. ✅

⚠️ **WARNING: `surface.keys` param name uncertainty.** The design routes `surface.keys`
to `cmuxSendKey(socket, surfaceID, key)` which calls cmux.sock `surface.send_key` with
param `key`. The CLI (`cli.go:75`) sends `surface.send_key` with flag `key`. This
appears consistent, but the RN client doesn't currently call `surface.keys` anywhere
in the reviewed code — it's a future method. Verify param names when implementing.

---

## FLOW 5: TOPOLOGY PUSH

**Trace:**

| Step | Actor | Detail | Verdict |
|------|-------|--------|---------|
| 1. Go detects change | design: `topologyWatcher.run()` | Polls `system.tree` every 2s, compares raw JSON bytes | ✅ PASS |
| 2. Go extracts workspaces | design: `ws_topology.go` | `tree.Windows[0].Workspaces` → raw JSON | ✅ PASS |
| 3. Go broadcasts | design | `{"event":"topology","data_base64":"<base64({workspaces:[...]})>"}` | ✅ PASS |
| 4. RN routing | `ws-client.ts:89-116` | No `ok` field → skips auth + response routing → forward to `onMessage` | ✅ PASS |
| 5. RN handling | `useConnection.ts:74-79` | `isEvent(msg)` → true. `case 'topology'`. `atob(data_base64)` → `{workspaces:[...]}`. `data.workspaces` → truthy. `store.setTopology(data.workspaces)`. | ✅ PASS |

**VERIFY: No id or stream_id on topology event.**

The topology event has only `event` and `data_base64` fields. The RN handler doesn't
check for `id` or `stream_id` — it goes straight to `data.workspaces`. ✅

**VERIFY: isEvent() returns true.**

`isEvent` checks `'event' in msg`. The topology event has `event: "topology"`. ✅

⚠️ **WARNING: First-window assumption.** Both Go (`tree.Windows[0].Workspaces`) and
RN (`tree.windows?.[0]?.workspaces`) assume a single window. The RN safely handles
missing data with optional chaining + `?? []`. The Go side (design spec) accesses
`tree.Windows[0]` without bounds checking — if cmux has zero windows, this panics.
Add bounds check in implementation.

---

## CROSS-CUTTING CONCERNS

### C1: base64 Encoding Consistency

The design spec explicitly addresses the base64 mismatch (§1.3):
> CRITICAL MISMATCH FIXED: The implementation plan puts raw JSON in data_base64.
> The client decodes with atob() (base64 decode). We MUST base64-encode the JSON payload.

The `encodeEventData` helper does `json.Marshal → base64.StdEncoding.EncodeToString`.
The RN decodes with `JSON.parse(atob(msg.data_base64))`.

✅ **PASS** — but note that Go's `base64.StdEncoding` uses standard alphabet with `=`
padding. JavaScript's `atob()` handles standard base64 with padding correctly. If the
implementation used `base64.RawStdEncoding` (no padding) or `base64.URLEncoding`
(different alphabet), `atob()` would fail. Ensure standard encoding is used.

### C2: Message Discrimination Safety

The `ServerMessage = RpcResponse | RpcEvent` union is discriminated by:
- `isResponse`: `'ok' in msg`
- `isEvent`: `'event' in msg`

These fields never overlap between Go's `rpcResponse` and `rpcEvent` structs. However,
both structs have an `error` field (different types: `*rpcError` vs `string`). This
doesn't affect discrimination since neither `isResponse` nor `isEvent` checks `error`.

✅ **PASS** — type guards are safe.

### C3: Concurrent Write Safety

The design notes (§1.1): "Conn.Write() IS goroutine-safe". Multiple goroutines
(readLoop responses, watchSurface events, topologyWatcher broadcasts) will call
`sendResponse`/`sendEvent` concurrently. The `coder/websocket` library handles this
via internal mutex.

✅ **PASS** — per library docs.

### C4: Connection Cleanup on Disconnect

Design flow: client disconnects → `readLoop` returns error → `cleanup()` cancels all
watcher contexts → watchSurface goroutines exit via `ctx.Done()` → `topologyWatcher.removeClient(cl)`.

✅ **PASS** — no goroutine leaks in the design.

### C5: Request Timeout vs Watcher Lifetime

`ws-client.ts:63-68` sets a 10s timeout on all `send()` promises. For `surface.subscribe`,
the ack response should arrive quickly (sub-100ms). But if cmux.sock is slow or the Go
server is under load, the timeout could fire before the ack arrives. This would:
1. Reject the subscribe promise (unhandled — see Flow 3 ⚠️)
2. Delete from `pendingRequests`
3. When the ack finally arrives, it has no matching pending → falls through to `onMessage`
4. `handleMessage` → `isEvent(msg)` → false (it's a response) → silently dropped

The subscribe still takes effect server-side. The watcher runs and pushes events.
The client receives screen events correctly. Only the ack confirmation is lost.

⚠️ **WARNING: Ack-loss is silent but non-fatal.** The subscribe still works.

---

## SUMMARY

| Flow | Status | Issues |
|------|--------|--------|
| **1: AUTH** | ✅ PASS | ⚠️ Silent auth failure → infinite reconnect loop |
| **2: SYSTEM.TREE** | ✅ PASS | ⚠️ Pseudocode doesn't explicitly show `ID: req.ID` |
| **3: SUBSCRIBE+STREAM** | ✅ PASS | ⚠️ Unhandled promise rejection on subscribe failure |
| **4: SURFACE.INPUT** | ✅ PASS | ⚠️ `surface.keys` params unverified (future method) |
| **5: TOPOLOGY PUSH** | ✅ PASS | ⚠️ First-window assumption panics on zero windows |

**Zero ❌ MISMATCH findings.** The design spec correctly matches the RN client protocol.

### Warnings (5 total, 0 blockers):

1. **Silent auth failure** — Bad token causes infinite reconnect with no user feedback.
   RN should detect repeated auth failures and surface an error.

2. **ID propagation in pseudocode** — The `handleProxiedRequest` pseudocode doesn't show
   `ID: req.ID` explicitly. Every response path must set it. Recommend adding a helper or
   asserting ID presence in tests.

3. **Unhandled promise rejection** — `client.send('surface.subscribe', ...)` not awaited/caught.
   Add `.catch(() => {})` or collect promises and `Promise.allSettled()`.

4. **`surface.keys` future method** — Param names (`surface_id`, `key`) match CLI usage
   but RN doesn't exercise this path yet. Verify when implementing.

5. **Zero-window panic** — `tree.Windows[0]` in topology watcher needs bounds check.
   RN handles this safely with `?.` but Go will index-out-of-bounds.

### Cross-cutting (5 items, all PASS):

- C1: base64 encoding — standard encoding compatible with `atob()` ✅
- C2: Message type discrimination — no field overlap ✅
- C3: Concurrent writes — library-level goroutine safety ✅
- C4: Cleanup on disconnect — no goroutine leaks ✅
- C5: Request timeout vs watcher lifetime — ack-loss silent but non-fatal ⚠️
