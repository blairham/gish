package p10k

import "testing"

// The prompt is re-rendered on every keystroke that changes the line, so
// this number is the one that decides whether the shell feels fast. It
// is a benchmark rather than a test because the budget belongs in
// docs/bench.md, but it is here so a regression is one command away.

func BenchmarkRenderLean(b *testing.B) {
	cfg := Preset("lean")
	ctx := sampleContext()
	b.ReportAllocs()
	for b.Loop() {
		fresh := *ctx // a real prompt gets a fresh context, and so its memo
		Render(cfg, &fresh)
	}
}

func BenchmarkRenderRainbow(b *testing.B) {
	cfg := Preset("rainbow")
	ctx := sampleContext()
	b.ReportAllocs()
	for b.Loop() {
		fresh := *ctx
		Render(cfg, &fresh)
	}
}

// BenchmarkRenderEverySegment loads the line up with every implemented
// segment — the worst case a configuration can ask for.
func BenchmarkRenderEverySegment(b *testing.B) {
	cfg := Preset("lean")
	cfg.SetList("RIGHT_PROMPT_ELEMENTS", Segments())
	ctx := sampleContext()
	b.ReportAllocs()
	for b.Loop() {
		fresh := *ctx
		Render(cfg, &fresh)
	}
}
