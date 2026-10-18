package codex

import (
	"sort"
	"strings"
)

// price is USD per million tokens.
type price struct {
	Input  float64
	Output float64
}

// prices is the cost estimate table keyed by model name prefix. The longest
// matching prefix wins. The Codex CLI reports tokens and no cost, so the
// harness estimates the cost from public list prices. Cached input tokens
// cost a tenth of the input price.
var prices = map[string]price{
	"gpt-5-nano":   {Input: 0.05, Output: 0.40},
	"gpt-5-mini":   {Input: 0.25, Output: 2},
	"gpt-5":        {Input: 1.25, Output: 10},
	"gpt-4.1-nano": {Input: 0.10, Output: 0.40},
	"gpt-4.1-mini": {Input: 0.40, Output: 1.60},
	"gpt-4.1":      {Input: 2, Output: 8},
	"gpt-4o-mini":  {Input: 0.15, Output: 0.60},
	"gpt-4o":       {Input: 2.50, Output: 10},
	"o4-mini":      {Input: 1.10, Output: 4.40},
	"o3-mini":      {Input: 1.10, Output: 4.40},
	"o3":           {Input: 2, Output: 8},
	"codex-mini":   {Input: 1.50, Output: 6},
}

// estimateCost estimates the USD cost of one turn. input is the total prompt
// size, cached tokens included. An unknown model costs 0.
func estimateCost(model string, input, cached, output int64) float64 {
	m := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "openai/"))
	keys := make([]string, 0, len(prices))
	for k := range prices {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if !strings.HasPrefix(m, k) {
			continue
		}
		p := prices[k]
		uncached := max(input-cached, 0)
		in := float64(uncached)*p.Input + float64(cached)*p.Input*0.1
		return (in + float64(output)*p.Output) / 1e6
	}
	return 0
}
