package dbusapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"kleinia.io/llmgw/internal/server"
	"kleinia.io/llmgw/internal/store"
)

// dispatchTree maps object paths to their interfaces. Every transport (bus
// and p2p) dispatches through the same table; methodEntry.fn receives the
// raw message plus the caller's sender name (for polkit).
type dispatchTree map[dbus.ObjectPath]map[string]*ifaceEntry

type ifaceEntry struct {
	methods map[string]*methodEntry
	xml     string // introspection fragment
}

type methodEntry struct {
	nArgs  int    // expected len(msg.Body)
	action string // polkit action id, "" = read-only method
	fn     func(g *Gateway, sender string, msg *dbus.Message) ([]any, *dbus.Error)
}

// invoke routes one method-call message through the dispatch tree: lookup,
// arity check, polkit gate, implementation. It is the single entry point
// shared by the p2p message loop and the bus-mode custom handler.
func (g *Gateway) invoke(msg *dbus.Message) ([]any, *dbus.Error) {
	path, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	ifaceName, _ := msg.Headers[dbus.FieldInterface].Value().(string)
	member, _ := msg.Headers[dbus.FieldMember].Value().(string)
	sender, _ := msg.Headers[dbus.FieldSender].Value().(string)

	if member == "" {
		e := dbus.MakeUnknownMethodError(member)
		return nil, &e
	}
	entry := g.lookup(path, ifaceName, member)
	if entry == nil {
		return nil, g.unknownDispatchError(path, ifaceName, member)
	}
	if len(msg.Body) != entry.nArgs {
		return nil, dbusError("org.freedesktop.DBus.Error.InvalidArgs",
			"%s.%s expects %d argument(s), got %d", ifaceName, member, entry.nArgs, len(msg.Body))
	}
	if entry.action != "" {
		if derr := g.authorize(sender, entry.action); derr != nil {
			return nil, derr
		}
	}
	return entry.fn(g, sender, msg)
}

func (g *Gateway) lookup(path dbus.ObjectPath, ifaceName, member string) *methodEntry {
	ifaces, ok := g.tree[path]
	if !ok {
		return nil
	}
	if ifaceName == "" { // interface-less call: search every interface
		for _, ie := range ifaces {
			if e, ok := ie.methods[member]; ok {
				return e
			}
		}
		return nil
	}
	if ie, ok := ifaces[ifaceName]; ok {
		return ie.methods[member]
	}
	return nil
}

func (g *Gateway) unknownDispatchError(path dbus.ObjectPath, ifaceName, member string) *dbus.Error {
	if ifaces, ok := g.tree[path]; ok && ifaceName != "" {
		if _, ok := ifaces[ifaceName]; !ok {
			e := dbus.MakeUnknownInterfaceError(ifaceName)
			return &e
		}
	}
	e := dbus.MakeUnknownMethodError(member)
	return &e
}

func dbusError(name, format string, a ...any) *dbus.Error {
	return &dbus.Error{Name: name, Body: []any{fmt.Sprintf(format, a...)}}
}

func invalidArgs(format string, a ...any) *dbus.Error {
	return dbusError("org.freedesktop.DBus.Error.InvalidArgs", format, a...)
}

// bodyX extracts typed arguments from a decoded method-call body.
func bodyString(args []any, i int) (string, *dbus.Error) {
	if s, ok := args[i].(string); ok {
		return s, nil
	}
	return "", invalidArgs("argument %d: expected string, got %T", i, args[i])
}

func bodyUint32(args []any, i int) (uint32, *dbus.Error) {
	if u, ok := args[i].(uint32); ok {
		return u, nil
	}
	return 0, invalidArgs("argument %d: expected uint32, got %T", i, args[i])
}

func bodyInt32(args []any, i int) (int32, *dbus.Error) {
	if v, ok := args[i].(int32); ok {
		return v, nil
	}
	return 0, invalidArgs("argument %d: expected int32, got %T", i, args[i])
}

func bodyDouble(args []any, i int) (float64, *dbus.Error) {
	if v, ok := args[i].(float64); ok {
		return v, nil
	}
	return 0, invalidArgs("argument %d: expected double, got %T", i, args[i])
}

// ---------------------------------------------------------------------------
// JSON wire shapes (secrets never appear in any of them)
// ---------------------------------------------------------------------------

type modelJSON struct {
	Alias    string `json:"alias"`
	Upstream string `json:"upstream"`
}

type healthJSON struct {
	State        string `json:"state"` // ok | cooling | disabled
	Fails        int    `json:"fails"`
	CoolingUntil string `json:"cooling_until"` // RFC3339, "" while healthy
}

type sourceJSON struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	BaseURL  string      `json:"base_url"`
	HasKey   bool        `json:"has_key"`
	Priority int         `json:"priority"`
	Weight   int         `json:"weight"`
	Enabled  bool        `json:"enabled"`
	Models   []modelJSON `json:"models"`
	Health   healthJSON  `json:"health"`
}

type addSourceReq struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	BaseURL  string      `json:"base_url"`
	APIKey   string      `json:"api_key"`
	Models   []modelJSON `json:"models"`
	Priority *int        `json:"priority"`
	Weight   *int        `json:"weight"`
}

type updateSourceReq struct {
	Name     *string      `json:"name"`
	Type     *string      `json:"type"`
	BaseURL  *string      `json:"base_url"`
	APIKey   *string      `json:"api_key"`
	Priority *int         `json:"priority"`
	Weight   *int         `json:"weight"`
	Enabled  *bool        `json:"enabled"`
	Models   *[]modelJSON `json:"models"`
}

type keyJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Suspended   bool   `json:"suspended"`
	Models      string `json:"models"`
	RPM         *int64 `json:"rpm"`
	TPM         *int64 `json:"tpm"`
	DailyTokens *int64 `json:"daily_tokens"`
	AID         string `json:"aid"`
	CreatedAt   string `json:"created_at"`
	RevokedAt   string `json:"revoked_at"`
}

type keyLimitsReq struct {
	Models      *string `json:"models"`
	RPM         *int64  `json:"rpm"`
	TPM         *int64  `json:"tpm"`
	DailyTokens *int64  `json:"daily_tokens"`
	AID         *string `json:"aid"`
}

type aggJSON struct {
	Label     string   `json:"label"`
	Requests  int64    `json:"requests"`
	InTokens  int64    `json:"in_tokens"`
	OutTokens int64    `json:"out_tokens"`
	CostEst   *float64 `json:"cost_est"`
}

type usageJSON struct {
	TS            string   `json:"ts"`
	ReqID         string   `json:"req_id"`
	KeyID         int64    `json:"key_id"`
	KeyName       string   `json:"key_name"`
	AID           string   `json:"aid"`
	SourceID      int64    `json:"source_id"`
	SourceName    string   `json:"source_name"`
	ClientFormat  string   `json:"client_format"`
	ModelAlias    string   `json:"model_alias"`
	UpstreamModel string   `json:"upstream_model"`
	In            int64    `json:"input_tokens"`
	Out           int64    `json:"output_tokens"`
	CacheRead     int64    `json:"cache_read_tokens"`
	CacheWrite    int64    `json:"cache_write_tokens"`
	Reasoning     int64    `json:"reasoning_tokens"`
	Cost          *float64 `json:"cost_est"`
	LatencyMS     int64    `json:"latency_ms"`
	TTFTMS        int64    `json:"ttft_ms"`
	Stream        bool     `json:"stream"`
	Status        int      `json:"status"`
	Degraded      bool     `json:"degraded"`
	Session       string   `json:"session"`
	ErrorKind     string   `json:"error_kind"`
}

type testResultJSON struct {
	OK         bool   `json:"ok"`
	LatencyMS  int64  `json:"latency_ms"`
	ModelCount int    `json:"model_count"`
	Error      string `json:"error"`
}

// dispatchTree construction --------------------------------------------------

func newDispatchTree() dispatchTree {
	tree := dispatchTree{
		Path: {
			ifaceManager: {methods: map[string]*methodEntry{}, xml: xmlManager1},
			ifaceSources: {
				xml: xmlSources1,
				methods: map[string]*methodEntry{
					"ListSources":  {nArgs: 0, fn: (*Gateway).mListSources},
					"AddSource":    {nArgs: 1, action: ActionManage, fn: (*Gateway).mAddSource},
					"UpdateSource": {nArgs: 2, action: ActionManage, fn: (*Gateway).mUpdateSource},
					"RemoveSource": {nArgs: 1, action: ActionManage, fn: (*Gateway).mRemoveSource},
					"SetPriority":  {nArgs: 2, action: ActionManage, fn: (*Gateway).mSetPriority},
					"SetWeight":    {nArgs: 2, action: ActionManage, fn: (*Gateway).mSetWeight},
					"TestSource":   {nArgs: 1, fn: (*Gateway).mTestSource},
				},
			},
			ifaceKeys: {
				xml: xmlKeys1,
				methods: map[string]*methodEntry{
					"ListKeys":   {nArgs: 0, fn: (*Gateway).mListKeys},
					"IssueKey":   {nArgs: 2, action: ActionGrant, fn: (*Gateway).mIssueKey},
					"UpdateKey":  {nArgs: 2, action: ActionManage, fn: (*Gateway).mUpdateKey},
					"SuspendKey": {nArgs: 1, action: ActionManage, fn: (*Gateway).mSuspendKey},
					"ResumeKey":  {nArgs: 1, action: ActionManage, fn: (*Gateway).mResumeKey},
					"RevokeKey":  {nArgs: 1, action: ActionManage, fn: (*Gateway).mRevokeKey},
				},
			},
			ifaceModels: {
				xml: xmlModels1,
				methods: map[string]*methodEntry{
					"ListPrices":  {nArgs: 0, fn: (*Gateway).mListPrices},
					"SetPrice":    {nArgs: 2, action: ActionManage, fn: (*Gateway).mSetPrice},
					"RemovePrice": {nArgs: 1, action: ActionManage, fn: (*Gateway).mRemovePrice},
				},
			},
			ifaceUsage: {
				xml: xmlUsage1,
				methods: map[string]*methodEntry{
					"Summary":    {nArgs: 1, fn: (*Gateway).mSummary},
					"Histogram":  {nArgs: 3, fn: (*Gateway).mHistogram},
					"Stats":      {nArgs: 1, fn: (*Gateway).mStats},
					"Recent":     {nArgs: 1, fn: (*Gateway).mRecent},
					"Logs":       {nArgs: 4, fn: (*Gateway).mLogs},
					"KeyWindows": {nArgs: 1, fn: (*Gateway).mKeyWindows},
				},
			},
			ifaceQuota: {methods: map[string]*methodEntry{}, xml: xmlQuota1},
			"org.freedesktop.DBus.Properties": {
				xml: xmlProperties,
				methods: map[string]*methodEntry{
					"Get":    {nArgs: 2, fn: (*Gateway).mPropGet},
					"GetAll": {nArgs: 1, fn: (*Gateway).mPropGetAll},
					"Set":    {nArgs: 3, fn: (*Gateway).mPropSet},
				},
			},
			"org.freedesktop.DBus.Introspectable": {
				xml: xmlIntrospectable,
				methods: map[string]*methodEntry{
					"Introspect": {nArgs: 0, fn: (*Gateway).mIntrospect},
				},
			},
			"org.freedesktop.DBus.Peer": {
				xml: xmlPeer,
				methods: map[string]*methodEntry{
					"Ping":         {nArgs: 0, fn: (*Gateway).mPing},
					"GetMachineId": {nArgs: 0, fn: (*Gateway).mGetMachineId},
				},
			},
		},
		// Minimal bus-daemon shim so bus-aware clients (godbus's Connect
		// performs Hello; GDBus sends AddMatch) work over a plain socket.
		dbusServicePath: {
			ifaceDBus: {
				xml: xmlDBusShim,
				methods: map[string]*methodEntry{
					"Hello":                      {nArgs: 0, fn: (*Gateway).mHello},
					"RequestName":                {nArgs: 2, fn: (*Gateway).mRequestName},
					"ReleaseName":                {nArgs: 1, fn: (*Gateway).mReleaseName},
					"AddMatch":                   {nArgs: 1, fn: (*Gateway).mNoReplyBus},
					"RemoveMatch":                {nArgs: 1, fn: (*Gateway).mNoReplyBus},
					"GetId":                      {nArgs: 0, fn: (*Gateway).mGetId},
					"ListNames":                  {nArgs: 0, fn: (*Gateway).mListNames},
					"ListActivatableNames":       {nArgs: 0, fn: (*Gateway).mListActivatableNames},
					"NameHasOwner":               {nArgs: 1, fn: (*Gateway).mNameHasOwner},
					"GetNameOwner":               {nArgs: 1, fn: (*Gateway).mGetNameOwner},
					"GetConnectionUnixProcessID": {nArgs: 1, fn: (*Gateway).mGetConnPID},
					"GetConnectionUnixUser":      {nArgs: 1, fn: (*Gateway).mGetConnUID},
				},
			},
			"org.freedesktop.DBus.Introspectable": {
				xml: xmlIntrospectable,
				methods: map[string]*methodEntry{
					"Introspect": {nArgs: 0, fn: (*Gateway).mIntrospect},
				},
			},
			"org.freedesktop.DBus.Peer": {
				xml: xmlPeer,
				methods: map[string]*methodEntry{
					"Ping":         {nArgs: 0, fn: (*Gateway).mPing},
					"GetMachineId": {nArgs: 0, fn: (*Gateway).mGetMachineId},
				},
			},
		},
		"/": {
			"org.freedesktop.DBus.Introspectable": {
				xml: xmlIntrospectable,
				methods: map[string]*methodEntry{
					"Introspect": {nArgs: 0, fn: (*Gateway).mIntrospect},
				},
			},
		},
	}
	return tree
}

// ---------------------------------------------------------------------------
// io.kleinia.Sources1
// ---------------------------------------------------------------------------

func (g *Gateway) mListSources(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	srcs, err := g.opts.Store.ListSources()
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "list sources: %v", err)
	}
	health := g.healthSnapshot()
	out := make([]string, 0, len(srcs))
	for _, src := range srcs {
		maps, err := g.opts.Store.SourceModels(src.ID)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "source %s models: %v", src.Name, err)
		}
		models := make([]modelJSON, 0, len(maps))
		for _, m := range maps {
			models = append(models, modelJSON{Alias: m.Alias, Upstream: m.UpstreamModel})
		}
		h := healthJSON{State: "ok"}
		if !src.Enabled {
			h.State = "disabled"
		}
		if hp, ok := health[src.ID]; ok && hp.state != "disabled" {
			h.State = hp.state
			h.Fails = hp.fails
		}
		b, err := json.Marshal(sourceJSON{
			ID: src.ID, Name: src.Name, Type: src.Type, BaseURL: src.BaseURL,
			HasKey: src.APIKey != "", Priority: src.Priority, Weight: src.Weight,
			Enabled: src.Enabled, Models: models, Health: h,
		})
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal source: %v", err)
		}
		out = append(out, string(b))
	}
	return []any{out}, nil
}

func (g *Gateway) mAddSource(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	raw, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	var req addSourceReq
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, invalidArgs("AddSource: bad json: %v", err)
	}
	if req.Name == "" || req.Type == "" || req.BaseURL == "" {
		return nil, invalidArgs("AddSource: name, type and base_url are required")
	}
	src := store.Source{Name: req.Name, Type: req.Type, BaseURL: req.BaseURL,
		APIKey: req.APIKey, Enabled: true}
	if req.Priority != nil {
		src.Priority = *req.Priority
	}
	if req.Weight != nil {
		src.Weight = *req.Weight
	}
	id, err := g.opts.Store.AddSource(src, modelMappings(req.Models))
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "add source: %v", err)
	}
	g.refreshSources()
	return []any{uint32(id)}, nil
}

func (g *Gateway) mUpdateSource(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	id, derr := bodyUint32(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	raw, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	var req updateSourceReq
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, invalidArgs("UpdateSource: bad json: %v", err)
	}
	src, models, ok := g.sourceByID(int64(id))
	if !ok {
		return nil, invalidArgs("UpdateSource: no source %d", id)
	}
	// Overlay only the fields the caller provided; api_key set to "" in the
	// json clears it, api_key absent keeps the stored one.
	if req.Name != nil {
		src.Name = *req.Name
	}
	if req.Type != nil {
		src.Type = *req.Type
	}
	if req.BaseURL != nil {
		src.BaseURL = *req.BaseURL
	}
	if req.APIKey != nil {
		src.APIKey = *req.APIKey
	}
	if req.Priority != nil {
		src.Priority = *req.Priority
	}
	if req.Weight != nil {
		src.Weight = *req.Weight
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	if req.Models != nil {
		models = modelMappings(*req.Models)
	}
	if err := g.opts.Store.UpdateSource(src, models); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "update source: %v", err)
	}
	g.refreshSources()
	return nil, nil
}

func (g *Gateway) mRemoveSource(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	id, derr := bodyUint32(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if err := g.opts.Store.DeleteSource(int64(id)); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "remove source: %v", err)
	}
	g.refreshSources()
	return nil, nil
}

func (g *Gateway) mSetPriority(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	return g.mutatePriorityWeight(msg, true)
}

func (g *Gateway) mSetWeight(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	return g.mutatePriorityWeight(msg, false)
}

func (g *Gateway) mutatePriorityWeight(msg *dbus.Message, priority bool) ([]any, *dbus.Error) {
	id, derr := bodyUint32(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	v, derr := bodyInt32(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	src, models, ok := g.sourceByID(int64(id))
	if !ok {
		return nil, invalidArgs("no source %d", id)
	}
	if priority {
		src.Priority = int(v)
	} else {
		src.Weight = int(v)
	}
	if err := g.opts.Store.UpdateSource(src, models); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "update source: %v", err)
	}
	g.refreshSources()
	return nil, nil
}

// testClient is the HTTP client for TestSource (10s per spec).
var testClient = &http.Client{Timeout: 10 * time.Second}

func (g *Gateway) mTestSource(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	id, derr := bodyUint32(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	src, _, ok := g.sourceByID(int64(id))
	if !ok {
		return nil, invalidArgs("TestSource: no source %d", id)
	}
	res := testResultJSON{OK: true}
	start := time.Now()
	url := strings.TrimRight(src.BaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		res.OK, res.Error = false, err.Error()
	} else {
		if src.APIKey != "" {
			if src.Type == "anthropic" {
				req.Header.Set("x-api-key", src.APIKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			} else {
				req.Header.Set("Authorization", "Bearer "+src.APIKey)
			}
		}
		resp, err := testClient.Do(req)
		if err != nil {
			res.OK, res.Error = false, err.Error()
		} else {
			defer resp.Body.Close()
			res.LatencyMS = time.Since(start).Milliseconds()
			if resp.StatusCode != http.StatusOK {
				res.OK, res.Error = false, fmt.Sprintf("HTTP %d", resp.StatusCode)
			} else {
				var parsed any
				if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
					res.OK, res.Error = false, err.Error()
				} else {
					res.ModelCount = countModels(parsed)
				}
			}
		}
	}
	if !res.OK {
		res.LatencyMS = time.Since(start).Milliseconds()
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal: %v", err)
	}
	return []any{string(b)}, nil
}

// countModels understands both the OpenAI {"data":[...]} shape and a bare
// model array.
func countModels(parsed any) int {
	switch v := parsed.(type) {
	case map[string]any:
		if d, ok := v["data"].([]any); ok {
			return len(d)
		}
	case []any:
		return len(v)
	}
	return 0
}

func modelMappings(models []modelJSON) []store.ModelMapping {
	out := make([]store.ModelMapping, 0, len(models))
	for _, m := range models {
		out = append(out, store.ModelMapping{Alias: m.Alias, UpstreamModel: m.Upstream})
	}
	return out
}

func (g *Gateway) sourceByID(id int64) (store.Source, []store.ModelMapping, bool) {
	srcs, err := g.opts.Store.ListSources()
	if err != nil {
		return store.Source{}, nil, false
	}
	for _, src := range srcs {
		if src.ID == id {
			models, err := g.opts.Store.SourceModels(id)
			if err != nil {
				return store.Source{}, nil, false
			}
			return src, models, true
		}
	}
	return store.Source{}, nil, false
}

// ---------------------------------------------------------------------------
// io.kleinia.Keys1
// ---------------------------------------------------------------------------

func (g *Gateway) mListKeys(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	keys, err := g.opts.Store.ListKeys()
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "list keys: %v", err)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		b, err := json.Marshal(keyJSON{
			ID: k.ID, Name: k.Name, Enabled: k.Enabled, Suspended: k.Suspended,
			Models: k.Models, RPM: k.RPM, TPM: k.TPM, DailyTokens: k.DailyTokens,
			AID: k.AID, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
		})
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal key: %v", err)
		}
		out = append(out, string(b))
	}
	return []any{out}, nil
}

func (g *Gateway) mIssueKey(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	app, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	limitsRaw, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	var req keyLimitsReq
	if limitsRaw != "" {
		if err := json.Unmarshal([]byte(limitsRaw), &req); err != nil {
			return nil, invalidArgs("IssueKey: bad limits json: %v", err)
		}
	}
	models := "*"
	if req.Models != nil && *req.Models != "" {
		models = *req.Models
	}
	k, err := g.opts.Store.AddKey(app, models, req.RPM, req.TPM, req.DailyTokens)
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "issue key: %v", err)
	}
	if req.AID != nil && *req.AID != "" {
		if err := g.opts.Store.BindKeyAID(app, *req.AID); err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "bind aid: %v", err)
		}
	}
	return []any{k.Key, uint32(k.ID)}, nil
}

// mUpdateKey patches one key's limits (models allow-list, rpm/tpm/daily,
// AID binding). Patch JSON uses the IssueKey shape: absent fields keep their
// current value; a limit of 0 clears it (unlimited); aid "" detaches.
func (g *Gateway) mUpdateKey(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	patchRaw, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	var req keyLimitsReq
	if patchRaw != "" {
		if err := json.Unmarshal([]byte(patchRaw), &req); err != nil {
			return nil, invalidArgs("UpdateKey: bad patch json: %v", err)
		}
	}
	if err := g.opts.Store.UpdateKey(name, req.Models, req.RPM, req.TPM,
		req.DailyTokens, req.AID); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "update key: %v", err)
	}
	return nil, nil
}

func (g *Gateway) keyAction(msg *dbus.Message, f func(name string) error) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if err := f(name); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "%v", err)
	}
	return nil, nil
}

func (g *Gateway) mSuspendKey(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	return g.keyAction(msg, func(name string) error { return g.opts.Store.SetKeySuspended(name, true) })
}

func (g *Gateway) mResumeKey(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	return g.keyAction(msg, func(name string) error { return g.opts.Store.SetKeySuspended(name, false) })
}

func (g *Gateway) mRevokeKey(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	return g.keyAction(msg, g.opts.Store.RevokeKey)
}

// ---------------------------------------------------------------------------
// io.kleinia.Models1 (per-alias pricing for cost estimation)
// ---------------------------------------------------------------------------

func (g *Gateway) pricer() (*server.Pricer, *dbus.Error) {
	if g.opts.Pricer == nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed",
			"price table not configured")
	}
	return g.opts.Pricer, nil
}

func (g *Gateway) mListPrices(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	pr, derr := g.pricer()
	if derr != nil {
		return nil, derr
	}
	out := make([]string, 0)
	for _, m := range pr.List() {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed",
				"marshal price: %v", err)
		}
		out = append(out, string(b))
	}
	return []any{out}, nil
}

// mSetPrice takes the price components as a JSON string: absent keys are
// "not enabled" for that model (e.g. no cache pricing).
//	{"input_per_mtok":0.14,"output_per_mtok":0.42,
//	 "cache_read_per_mtok":0.014,"cache_write_per_mtok":0.175}
func (g *Gateway) mSetPrice(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	pr, derr := g.pricer()
	if derr != nil {
		return nil, derr
	}
	alias, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	raw, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	var mp server.ModelPrice
	if err := json.Unmarshal([]byte(raw), &mp); err != nil {
		return nil, invalidArgs("SetPrice: bad price json: %v", err)
	}
	if err := pr.Set(alias, mp); err != nil {
		return nil, invalidArgs("SetPrice: %v", err)
	}
	return nil, nil
}

func (g *Gateway) mRemovePrice(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	pr, derr := g.pricer()
	if derr != nil {
		return nil, derr
	}
	alias, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if err := pr.Remove(alias); err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed",
			"remove price: %v", err)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// io.kleinia.Usage1 (read-only, no gating)
// ---------------------------------------------------------------------------

func (g *Gateway) mSummary(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	since, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return nil, invalidArgs("Summary: since must be RFC3339: %v", err)
	}
	byKey, byModel, bySource, byAid, err := g.opts.Store.SummarySince(since)
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "summary: %v", err)
	}
	out := make([]string, 0, 4)
	for _, group := range [][]store.Agg{byKey, byModel, bySource, byAid} {
		b, err := marshalAggs(group)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "summary: %v", err)
		}
		out = append(out, b)
	}
	return []any{out}, nil
}

func (g *Gateway) mHistogram(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	since, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	bucket, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	dimension, derr := bodyString(msg.Body, 2)
	if derr != nil {
		return nil, derr
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return nil, invalidArgs("Histogram: since must be RFC3339: %v", err)
	}
	buckets, err := g.opts.Store.Histogram(since, bucket, dimension)
	if err != nil {
		return nil, invalidArgs("Histogram: %v", err)
	}
	out := make([]string, 0, len(buckets))
	for _, b := range buckets {
		j, err := json.Marshal(b)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "histogram: %v", err)
		}
		out = append(out, string(j))
	}
	return []any{out}, nil
}

func (g *Gateway) mStats(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	since, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return nil, invalidArgs("Stats: since must be RFC3339: %v", err)
	}
	st, err := g.opts.Store.Stats(since)
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "stats: %v", err)
	}
	j, err := json.Marshal(st)
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "stats: %v", err)
	}
	return []any{string(j)}, nil
}

func marshalAggs(aggs []store.Agg) (string, error) {
	out := make([]aggJSON, 0, len(aggs))
	for _, a := range aggs {
		out = append(out, aggJSON{Label: a.Label, Requests: a.Requests,
			InTokens: a.InTokens, OutTokens: a.OutTokens, CostEst: a.CostEst})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mLogs is the paginated call-log surface: one page of rows plus the total
// matching count. filter JSON (all optional):
//	{"ident":"opencode","model":"m1","source":"primary",
//	 "search":"timeout","errors":true}
func (g *Gateway) mLogs(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	since, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	offset, derr := bodyUint32(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	limit, derr := bodyUint32(msg.Body, 2)
	if derr != nil {
		return nil, derr
	}
	filterRaw, derr := bodyString(msg.Body, 3)
	if derr != nil {
		return nil, derr
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return nil, invalidArgs("Logs: since must be RFC3339: %v", err)
	}
	f := store.UsageFilter{Since: since}
	if filterRaw != "" {
		var req struct {
			Ident   string `json:"ident"`
			Model   string `json:"model"`
			Source  string `json:"source"`
			Search  string `json:"search"`
			Errors  *bool  `json:"errors"`
		}
		if err := json.Unmarshal([]byte(filterRaw), &req); err != nil {
			return nil, invalidArgs("Logs: bad filter json: %v", err)
		}
		f.Ident, f.Model, f.Source = req.Ident, req.Model, req.Source
		f.Search, f.HasError = req.Search, req.Errors
	}
	rows, total, err := g.opts.Store.UsagePage(f, int(offset), int(limit))
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "logs: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		u := usageJSON{
			TS: r.TS, ReqID: r.ReqID, KeyID: r.KeyID, KeyName: r.KeyName, AID: r.AID,
			SourceID: r.SourceID, SourceName: r.SourceName, ClientFormat: r.ClientFormat,
			ModelAlias: r.ModelAlias, UpstreamModel: r.UpstreamModel,
			In: r.In, Out: r.Out, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Reasoning: r.Reasoning, Cost: r.Cost, LatencyMS: r.LatencyMS, TTFTMS: r.TTFTMS,
			Stream: r.Stream, Status: r.Status, Degraded: r.Degraded,
			Session: r.Session, ErrorKind: r.ErrorKind,
		}
		b, err := json.Marshal(u)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal row: %v", err)
		}
		out = append(out, string(b))
	}
	return []any{out, uint32(total)}, nil
}

func (g *Gateway) mRecent(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	limit, derr := bodyUint32(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	rows, err := g.opts.Store.RecentUsage(int(limit))
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "recent usage: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		u := usageJSON{
			TS: r.TS, ReqID: r.ReqID, KeyID: r.KeyID, KeyName: r.KeyName, AID: r.AID,
			SourceID: r.SourceID, SourceName: r.SourceName, ClientFormat: r.ClientFormat,
			ModelAlias: r.ModelAlias, UpstreamModel: r.UpstreamModel,
			In: r.In, Out: r.Out, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Reasoning: r.Reasoning, Cost: r.Cost, LatencyMS: r.LatencyMS, TTFTMS: r.TTFTMS,
			Stream: r.Stream, Status: r.Status, Degraded: r.Degraded,
			Session: r.Session, ErrorKind: r.ErrorKind,
		}
		b, err := json.Marshal(u)
		if err != nil {
			return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal row: %v", err)
		}
		out = append(out, string(b))
	}
	return []any{out}, nil
}

func (g *Gateway) mKeyWindows(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	ident, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	var (
		rpm, tpm, daily int64
		limits          struct {
			RPM         *int64 `json:"rpm"`
			TPM         *int64 `json:"tpm"`
			DailyTokens *int64 `json:"daily_tokens"`
		}
		err error
	)
	switch {
	case strings.HasPrefix(ident, "key:"):
		name := strings.TrimPrefix(ident, "key:")
		k, _, ok := g.keyByName(name)
		if !ok {
			return nil, invalidArgs("KeyWindows: no key %q", name)
		}
		rpm, tpm, daily, err = g.opts.Store.KeyWindows(k.ID)
		limits.RPM, limits.TPM, limits.DailyTokens = k.RPM, k.TPM, k.DailyTokens
	case strings.HasPrefix(ident, "aid:"):
		aid := strings.TrimPrefix(ident, "aid:")
		rpm, tpm, daily, err = g.opts.Store.AidWindows(aid)
		if k, kerr := g.opts.Store.FindKeyByAID(aid); kerr == nil {
			limits.RPM, limits.TPM, limits.DailyTokens = k.RPM, k.TPM, k.DailyTokens
		}
	default:
		return nil, invalidArgs("KeyWindows: ident must be key:<name> or aid:<aid>")
	}
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "windows: %v", err)
	}
	b, err := json.Marshal(struct {
		RPM    int64 `json:"rpm"`
		TPM    int64 `json:"tpm"`
		Daily  int64 `json:"daily"`
		Limits struct {
			RPM         *int64 `json:"rpm"`
			TPM         *int64 `json:"tpm"`
			DailyTokens *int64 `json:"daily_tokens"`
		} `json:"limits"`
	}{rpm, tpm, daily, limits})
	if err != nil {
		return nil, dbusError("org.freedesktop.DBus.Error.Failed", "marshal: %v", err)
	}
	return []any{string(b)}, nil
}

func (g *Gateway) keyByName(name string) (store.Key, bool, bool) {
	keys, err := g.opts.Store.ListKeys()
	if err != nil {
		return store.Key{}, false, false
	}
	for _, k := range keys {
		if k.Name == name {
			return k, k.Enabled, true
		}
	}
	return store.Key{}, false, false
}

// ---------------------------------------------------------------------------
// org.freedesktop.DBus.Properties (Manager1's read-only properties)
// ---------------------------------------------------------------------------

func (g *Gateway) propValue(iface, name string) (dbus.Variant, bool) {
	if iface == ifaceManager {
		switch name {
		case "Version":
			return dbus.MakeVariant(APIVer), true
		case "Backend":
			return dbus.MakeVariant(Backend), true
		case "HTTPAddr":
			return dbus.MakeVariant(g.opts.HTTPAddr), true
		}
	}
	return dbus.Variant{}, false
}

func (g *Gateway) mPropGet(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	iface, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	prop, derr := bodyString(msg.Body, 1)
	if derr != nil {
		return nil, derr
	}
	v, ok := g.propValue(iface, prop)
	if !ok {
		return nil, invalidArgs("no property %s.%s", iface, prop)
	}
	return []any{v}, nil
}

func (g *Gateway) mPropGetAll(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	iface, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	props := map[string]dbus.Variant{}
	for _, name := range []string{"Version", "Backend", "HTTPAddr"} {
		if v, ok := g.propValue(iface, name); ok {
			props[name] = v
		}
	}
	return []any{props}, nil
}

func (g *Gateway) mPropSet(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return nil, dbusError("org.freedesktop.DBus.Error.PropertyReadOnly",
		"Manager1 properties are read-only")
}

// ---------------------------------------------------------------------------
// Introspection, Peer and the p2p bus shim
// ---------------------------------------------------------------------------

func (g *Gateway) mIntrospect(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	path, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	var b strings.Builder
	b.WriteString("<node>\n")
	if ifaces, ok := g.tree[path]; ok {
		names := make([]string, 0, len(ifaces))
		for name := range ifaces {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b.WriteString(ifaces[name].xml)
			b.WriteString("\n")
		}
	}
	for _, child := range childNodes(g.tree, path) {
		fmt.Fprintf(&b, "  <node name=%q/>\n", child)
	}
	b.WriteString("</node>")
	return []any{b.String()}, nil
}

func childNodes(tree dispatchTree, path dbus.ObjectPath) []string {
	prefix := string(path)
	if prefix != "/" {
		prefix += "/"
	}
	set := map[string]struct{}{}
	for p := range tree {
		after, ok := strings.CutPrefix(string(p), prefix)
		if !ok || after == "" {
			continue
		}
		name, _, _ := strings.Cut(after, "/")
		if name != "" {
			set[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (g *Gateway) mPing(_ string, _ *dbus.Message) ([]any, *dbus.Error) { return nil, nil }

func (g *Gateway) mGetMachineId(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{g.guid}, nil
}

func (g *Gateway) mHello(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{":1.0"}, nil
}

func (g *Gateway) mRequestName(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	g.mu.Lock()
	g.names[name] = true
	g.mu.Unlock()
	return []any{uint32(dbus.RequestNameReplyPrimaryOwner)}, nil
}

func (g *Gateway) mReleaseName(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	g.mu.Lock()
	delete(g.names, name)
	g.mu.Unlock()
	return []any{uint32(dbus.ReleaseNameReplyReleased)}, nil
}

func (g *Gateway) mNoReplyBus(_ string, _ *dbus.Message) ([]any, *dbus.Error) { return nil, nil }

func (g *Gateway) mGetId(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{g.guid}, nil
}

func (g *Gateway) mListNames(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	g.mu.Lock()
	names := make([]string, 0, len(g.names)+2)
	names = append(names, ifaceDBus, BusName)
	for name := range g.names {
		names = append(names, name)
	}
	g.mu.Unlock()
	return []any{names}, nil
}

func (g *Gateway) mListActivatableNames(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{[]string{}}, nil
}

func (g *Gateway) mNameHasOwner(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	g.mu.Lock()
	_, owned := g.names[name]
	g.mu.Unlock()
	return []any{owned || name == BusName}, nil
}

func (g *Gateway) mGetNameOwner(_ string, msg *dbus.Message) ([]any, *dbus.Error) {
	name, derr := bodyString(msg.Body, 0)
	if derr != nil {
		return nil, derr
	}
	if name == BusName {
		return []any{":1.0"}, nil
	}
	return nil, dbusError("org.freedesktop.DBus.Error.NameHasNoOwner",
		"no such name %q", name)
}

func (g *Gateway) mGetConnPID(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{uint32(os.Getpid())}, nil
}

func (g *Gateway) mGetConnUID(_ string, _ *dbus.Message) ([]any, *dbus.Error) {
	return []any{uint32(os.Getuid())}, nil
}
