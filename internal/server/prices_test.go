package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kleinia.io/llmgw/internal/ir"
)

func fp(v float64) *float64 { return &v }

func almostEq(got, want float64) bool { return got > want-1e-9 && got < want+1e-9 }

// TestCostFourComponents 验证四分量计价公式（IR 语义：cache read 是
// input 的切出量；cache write 是独立维度）：
//
//	in=0.14 out=0.42 cr=0.014 cw=0.175（USD / 1M tokens）
//	Usage{in=1_200_000, out=800_000, cr=200_000, cw=100_000}
//	→ (1_200_000−200_000)×0.14 + 200_000×0.014 + 800_000×0.42
//	  + 100_000×0.175 = 0.14 + 0.0028 + 0.336 + 0.0175 = 0.4963
func TestCostFourComponents(t *testing.T) {
	p := LoadPrices("")
	if err := p.Set("m", ModelPrice{
		InputPerMTok: fp(0.14), OutputPerMTok: fp(0.42),
		CacheReadPerMTok: fp(0.014), CacheWritePerMTok: fp(0.175),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c := p.Cost("m", ir.Usage{
		InputTokens: 1_200_000, OutputTokens: 800_000,
		CacheReadTokens: 200_000, CacheWriteTokens: 100_000,
	})
	if c == nil || !almostEq(*c, 0.4963) {
		t.Fatalf("four-component cost = %v, want 0.4963", c)
	}
}

// TestCostDisabledComponents：未启用分量的语义。
func TestCostDisabledComponents(t *testing.T) {
	p := LoadPrices("")
	// 只有 in/out：缓存读并入输入价（保守），缓存写不计费
	if err := p.Set("plain", ModelPrice{
		InputPerMTok: fp(0.1), OutputPerMTok: fp(0.4),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c := p.Cost("plain", ir.Usage{
		InputTokens: 1_000_000, OutputTokens: 500_000,
		CacheReadTokens: 400_000, CacheWriteTokens: 300_000,
	})
	// 1M 全部按输入价 0.1（含 400k 缓存读）+ 0.5M×0.4；缓存写 0
	if c == nil || !almostEq(*c, 0.1+0.2) {
		t.Fatalf("in/out-only cost = %v, want 0.3", c)
	}
	// 单独启用缓存读（切出）：无输入价时输入本体不计费
	if err := p.Set("cr-only", ModelPrice{
		CacheReadPerMTok: fp(0.01),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c = p.Cost("cr-only", ir.Usage{
		InputTokens: 1_000_000, CacheReadTokens: 300_000, OutputTokens: 9,
	})
	if c == nil || !almostEq(*c, 0.003) {
		t.Fatalf("cache-read-only cost = %v, want 0.003", c)
	}
	// 免费价（0）是合法配置
	if err := p.Set("free", ModelPrice{InputPerMTok: fp(0)}); err != nil {
		t.Fatalf("free pricing rejected: %v", err)
	}
	if c := p.Cost("free", ir.Usage{InputTokens: 5_000_000}); c == nil || *c != 0 {
		t.Fatalf("free cost = %v, want 0", c)
	}
	// 未定价 → nil（绝不猜价）
	if got := p.Cost("unknown", ir.Usage{}); got != nil {
		t.Fatalf("unpriced cost = %v, want nil", *got)
	}
}

func TestPricerValidation(t *testing.T) {
	p := LoadPrices("")
	if err := p.Set("", ModelPrice{InputPerMTok: fp(1)}); err == nil {
		t.Fatal("empty alias accepted")
	}
	if err := p.Set("m", ModelPrice{InputPerMTok: fp(-1)}); err == nil {
		t.Fatal("negative input accepted")
	}
	if err := p.Set("m", ModelPrice{CacheWritePerMTok: fp(-0.1)}); err == nil {
		t.Fatal("negative cache write accepted")
	}
	if err := p.Set("m", ModelPrice{}); err == nil {
		t.Fatal("no-component price accepted")
	}
}

func TestPricerSetRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	p := LoadPrices(path)

	if got := p.Cost("m1", ir.Usage{InputTokens: 1000}); got != nil {
		t.Fatalf("cost before pricing = %v, want nil", *got)
	}
	// 热生效：不重启即影响后续请求
	if err := p.Set("m1", ModelPrice{
		InputPerMTok: fp(0.5), OutputPerMTok: fp(1.5),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c := p.Cost("m1", ir.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if c == nil || !almostEq(*c, 2.0) {
		t.Fatalf("cost after Set = %v, want 2.0", c)
	}
	// 落盘并在重载后保留（旧形状：无缓存分量的两键 JSON）
	p2 := LoadPrices(path)
	c2 := p2.Cost("m1", ir.Usage{InputTokens: 1_000_000})
	if c2 == nil || !almostEq(*c2, 0.5) {
		t.Fatalf("cost after reload = %v, want 0.5", c2)
	}
	// 文件形状：{"models": {alias: {启用的分量…}}}，未启用分量不出现
	raw, _ := os.ReadFile(path)
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("persisted shape: %v (%s)", err, raw)
	}
	m1 := shape["models"].(map[string]any)["m1"].(map[string]any)
	if m1["input_per_mtok"] != float64(0.5) {
		t.Fatalf("persisted input price wrong: %s", raw)
	}
	for _, absent := range []string{"cache_read_per_mtok", "cache_write_per_mtok"} {
		if _, ok := m1[absent]; ok {
			t.Fatalf("disabled component %q persisted: %s", absent, raw)
		}
	}
	// 四分量落盘往返
	if err := p2.Set("m2", ModelPrice{
		InputPerMTok: fp(1), CacheWritePerMTok: fp(2),
	}); err != nil {
		t.Fatalf("Set m2: %v", err)
	}
	p3 := LoadPrices(path)
	c3 := p3.Cost("m2", ir.Usage{
		InputTokens: 2_000_000, CacheWriteTokens: 1_000_000,
	})
	if c3 == nil || !almostEq(*c3, 2.0+2.0) {
		t.Fatalf("m2 cost after reload = %v, want 4.0", c3)
	}
	// Remove：成本回到未知并从文件消失
	if err := p3.Remove("m1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := p3.Cost("m1", ir.Usage{}); got != nil {
		t.Fatalf("cost after Remove = %v, want nil", *got)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != "" && jsonToMap(t, raw)["models"].(map[string]any)["m1"] != nil {
		t.Fatalf("m1 still in file after Remove: %s", raw)
	}
}

func jsonToMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad json %s: %v", raw, err)
	}
	return m
}

func TestPricerListSorted(t *testing.T) {
	p := LoadPrices("")
	_ = p.Set("b", ModelPrice{InputPerMTok: fp(1)})
	_ = p.Set("a", ModelPrice{OutputPerMTok: fp(1)})
	list := p.List()
	if len(list) != 2 || list[0].Alias != "a" || list[1].Alias != "b" {
		t.Fatalf("List = %v", list)
	}
}
