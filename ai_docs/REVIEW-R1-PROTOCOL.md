# Review R1: Protocol Struct Alignment (Go ↔ React Native)

> **Reviewer:** cmux/crew/designer
> **Date:** 2026-03-21
> **Scope:** Go structs in `main.go` + design spec (`WS-TRANSPORT-DESIGN.md`) vs RN types in `protocol.ts`, `store.ts`, `ws-client.ts`, `useConnection.ts`
> **Verdict:** 1 critical mismatch, 1 warning, 4 passes

---

## Check 1: rpcRequest — Go `json:"id"` (any) vs TS `id?: string`

### ✅ PASS

**ID type:** Go uses `any`, TS uses `string`. No mismatch — the TS client
always sends string IDs (`String(this.nextId++)` in `ws-client.ts:60`), and
the auth message omits `id` entirely. Go's `any` holds a string at runtime;
`nil` when absent. Both directions work.

**Params:** Go has `Params map[string]any` with no `omitempty`. TS has
`params?: Record<string, unknown>`. The `rpcRequest` flows **TS → Go only**
(client sends, server receives). When TS omits `params` (e.g.,
`client.send('system.tree')` with no second arg), Go deserializes the missing
field as a `nil` map. Indexing a nil map in Go returns the zero value without
panicking — safe.

---

## Check 2: rpcResponse — Go `json:"id,omitempty"` (any) vs TS `id?: string`

### ✅ PASS

**Auth response ID omission:** The critical invariant is that the auth
response MUST NOT include `id`, because the client checks
`isResponse(msg) && !msg.id && msg.ok` (`ws-client.ts:93`).

Go behavior: `rpcResponse{OK: true, Result: ...}` leaves `ID` as `nil`
(zero value for `any`). `json.Marshal` with `omitempty` on `any` considers
`nil` the zero value and **omits the field**. Verified: `json.Marshal` of
`rpcResponse{ID: nil}` produces `{"ok":true,...}` with no `"id"` key.

The client's `!msg.id` evaluates to `true` when the field is absent from
JSON (TypeScript resolves the missing key as `undefined`, which is falsy). ✅

**Non-auth responses:** Client sends `id: "1"` → Go reads string into `any`
→ copies to `rpcResponse.ID` → marshals as `"id":"1"`. Client matches via
`pendingRequests.get(String(msg.id))` (`ws-client.ts:98`). ✅

---

## Check 3: rpcEvent — Go `json:"stream_id,omitempty"` vs TS `stream_id?: string`

### ✅ PASS

**screen_init / screen_diff:** Design spec (ws_stream.go, lines 272-293)
explicitly sets `StreamID: surfaceID` on both event types. The client reads
`msg.stream_id` as the key into `store.screens` (`useConnection.ts:84,91`). ✅

**topology:** Design spec (ws_topology.go, line 411-415) constructs the
topology event without setting `StreamID`. Go's `omitempty` on `string`
omits the empty string `""`. Client topology handler does NOT read
`stream_id` — it reads `data.workspaces` directly (`useConnection.ts:77`). ✅

---

## Check 4: base64 encoding — Go `base64.StdEncoding` vs JS `atob()`

### ✅ PASS

**Go side:** `encodeEventData()` uses `base64.StdEncoding.EncodeToString()`.
`StdEncoding` uses the RFC 4648 standard alphabet: `A-Za-z0-9+/` with `=`
padding.

**TS side:** `atob()` (available in React Native) decodes standard base64
with the same alphabet and padding. This is the WHATWG spec implementation.

**Compatibility confirmed:** `StdEncoding` (Go) ↔ `atob()` (JS) use
identical alphabets. If the design had used `base64.URLEncoding` (which
substitutes `-_` for `+/`), `atob()` would silently produce corrupt output.
The design correctly chose `StdEncoding`.

---

## Check 5: lineChange — Go `json:"line,omitempty"` on `int` vs TS `change.line !== undefined`

### ❌ MISMATCH — line=0 is silently dropped

**The bug:** Go's `omitempty` on `int` considers `0` the zero value and
**omits the field from JSON**. Line index 0 (the first line of the terminal
screen) is a valid, common value.

**Go struct (design spec, ws_diff.go):**
```go
type lineChange struct {
    Op    string `json:"op"`
    Line  int    `json:"line,omitempty"`   // ← 0 is omitted!
    Text  string `json:"text,omitempty"`
    Count int    `json:"count,omitempty"`  // ← 0 is omitted (but count=0 is nonsensical)
}
```

**Scenario — replacing the first terminal line:**
```
Go builds:  lineChange{Op: "replace", Line: 0, Text: "$ whoami"}
Marshals:   {"op":"replace","text":"$ whoami"}          ← no "line" field!
TS receives: change.line === undefined
TS guard:    change.op === 'replace' && change.line !== undefined  → FALSE
Result:      Replace is SILENTLY SKIPPED. First line never updates.
```

**Affected operations:**
| Operation | Impact |
|-----------|--------|
| `replace` at line 0 | First terminal line never updates |
| `delete` at line 0 | Cannot delete from the top of the screen |
| `delete` with count 0 | Not a real issue (deleting 0 lines is a no-op) |

**Impact:** Every terminal screen starts at line 0. The prompt line, status
bars, and any content at the top of the screen will appear frozen after the
initial `screen_init`. This is a **high-severity visual bug** that affects
every terminal session.

**Fix:** Remove `omitempty` from the `Line` field, or use `*int` (pointer)
so that `nil` means "absent" and `0` means "line zero":

```go
// Option A: Always include line (simplest)
Line  int    `json:"line"`

// Option B: Pointer semantics (if line must sometimes be absent)
Line  *int   `json:"line,omitempty"`
```

**Recommendation:** Option A. The `append` operation doesn't use `line`
(it appends to the end), so receiving `"line":0` when it's not needed is
harmless — the TS client only reads `change.line` inside the op-specific
branches.

---

## Check 6: topology event — json.RawMessage wrapping

### ✅ PASS

**Flow:**
1. `cmuxTopology()` returns `json.RawMessage` (raw TreeResult JSON)
2. Code unmarshals into struct with `Workspaces json.RawMessage` field
3. Extracts `tree.Windows[0].Workspaces` → still `json.RawMessage`
4. Wraps: `map[string]any{"workspaces": workspaces}`
5. `encodeEventData()` → `json.Marshal` → base64

**Double-encoding concern:** When `json.RawMessage` is stored in a
`map[string]any`, Go preserves the concrete type. `json.Marshal` checks
if the value implements `json.Marshaler` — `json.RawMessage` does, via
its `MarshalJSON()` method which returns the raw bytes verbatim.

If `json.RawMessage` were treated as `[]byte` (its underlying type),
`json.Marshal` would base64-encode it (Go's default for byte slices).
But the `json.Marshaler` interface takes precedence, so the workspaces
array is embedded literally.

**Result:** `{"workspaces":[{...ws1...},{...ws2...}]}` → base64 → sent.
Client decodes: `JSON.parse(atob(...))` → `{workspaces: [...]}` →
`store.setTopology(data.workspaces)`. ✅

**Caveat:** This relies on the `json.RawMessage` concrete type being
preserved through the `map[string]any` value. If any intermediate step
re-unmarshals `workspaces` into `map[string]any` or `[]any`, the
`json.RawMessage` guarantee is lost. The design spec's approach of
extracting via struct field (not generic unmarshal) is correct.

---

## Summary

| # | Check | Result |
|---|-------|--------|
| 1 | rpcRequest ID type + params handling | ✅ PASS |
| 2 | rpcResponse auth ID omission | ✅ PASS |
| 3 | rpcEvent stream_id presence/absence | ✅ PASS |
| 4 | base64 encoding compatibility | ✅ PASS |
| 5 | lineChange line=0 omitted by omitempty | ❌ MISMATCH |
| 6 | topology json.RawMessage wrapping | ✅ PASS |

**Critical finding:** Check 5 is a **latent bug** in the design spec that
will cause the first line of every terminal screen to appear frozen. The
fix is trivial (remove `omitempty` from `Line` field) but must be applied
before implementation.
