package tmuxctl

import (
	"strings"
	"testing"
)

func TestNormalizeSnapshot(t *testing.T) {
	t.Parallel()

	raw := `╭─────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.116.0-alpha.1)          │
│                                             │
│ model:     gpt-5.4 xhigh   /model to change │
│ directory: ~/tools                          │
╰─────────────────────────────────────────────╯

  Tip: New Build faster with the Codex App.

› hi

• Hi.

	› 在是？

• 在。说事。

› Write tests for @filename

  gpt-5.4 xhigh · 100% left · ~/tools
`

	got := NormalizeSnapshot(strings.ReplaceAll(raw, "\t", ""))
	want := "› hi\n\n• Hi.\n\n› 在是？\n\n• 在。说事。\n\n› Write tests for @filename"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestNormalizeSnapshotKeepsConsoleBodyIntact(t *testing.T) {
	t.Parallel()

	raw := `› 哈哈，你能看到我在发什么吗？

• 能看到。你发的是：“哈哈，你能看到我在发什么吗？”

› 1. 今天星期几？
  2. 假如你是小a，你叫什么名字？

  4. 好了。

• 你在问两个直接问题，我先用系统时间确认今天的具体日期和星期，再一起回答。

• Ran TZ=Asia/Shanghai date '+%Y-%m-%d %A'
  └ 2026-03-20 Friday

────────────────────────────────────────────────────────────────────────────────

	• 1. 今天是星期五，日期是 2026 年 3 月 20 日。
  2. 假如我是小a，那我就叫小a。
`

	got := NormalizeSnapshot(strings.ReplaceAll(raw, "\t", ""))
	want := "› 哈哈，你能看到我在发什么吗？\n\n• 能看到。你发的是：“哈哈，你能看到我在发什么吗？”\n\n› 1. 今天星期几？\n  2. 假如你是小a，你叫什么名字？\n\n  4. 好了。\n\n• 你在问两个直接问题，我先用系统时间确认今天的具体日期和星期，再一起回答。\n\n• Ran TZ=Asia/Shanghai date '+%Y-%m-%d %A'\n  └ 2026-03-20 Friday\n\n────────────────────────────────────────────────────────────────────────────────\n\n• 1. 今天是星期五，日期是 2026 年 3 月 20 日。\n  2. 假如我是小a，那我就叫小a。"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestDiffTextUsesLineOverlap(t *testing.T) {
	t.Parallel()

	prev := "• Alpha\n• Beta"
	curr := "• Beta\n• Gamma"
	if got, want := DiffText(prev, curr), "• Gamma"; got != want {
		t.Fatalf("DiffText() = %q, want %q", got, want)
	}
}

func TestDiffTextReturnsWholeSnapshotOnReset(t *testing.T) {
	t.Parallel()

	prev := "• One"
	curr := "• Two"
	if got, want := DiffText(prev, curr), curr; got != want {
		t.Fatalf("DiffText() = %q, want %q", got, want)
	}
}

func TestSliceAfterDropsBaselinePrefix(t *testing.T) {
	t.Parallel()

	base := "› Summarize recent commits\n\n› Explain this code"
	curr := "› Summarize recent commits\n\n› Explain this code\n\n› hello\n\n• hi"
	if got, want := SliceAfter(base, curr), "› hello\n\n• hi"; got != want {
		t.Fatalf("SliceAfter() = %q, want %q", got, want)
	}
}

func TestAppendOnlyDeltaDetectsRewrite(t *testing.T) {
	t.Parallel()

	prev := "› hello\n\n• partial"
	curr := "› hello\n\n• rewritten\n\n• final"
	if got, ok := AppendOnlyDelta(prev, curr); ok || got != "" {
		t.Fatalf("AppendOnlyDelta() = (%q, %v), want rewrite signal", got, ok)
	}
}

func TestOutputBodyDropsPromptBlocks(t *testing.T) {
	t.Parallel()

	text := "› 哈哈，你能看到我在发什么吗？\n\n  1. 今天星期几？\n  2. 假如你是小a，你叫什么名字？\n\n  4. 好了。\n\n• 我先核对一下当前系统日期，再直接回答这两个问题。\n\n• Ran date '+%Y-%m-%d %A %u %Z'\n  └ 2026-03-20 Friday 5 CST\n\n────────────────────────────────────────────────────────────────────────────────\n\n• 能看到。\n\n  1. 今天是星期五。\n  2. 假如我是小a，那我就叫小a。\n  3. 好了。\n\n› Implement {feature}"

	got := OutputBody(text)
	want := "• 我先核对一下当前系统日期，再直接回答这两个问题。\n\n• Ran date '+%Y-%m-%d %A %u %Z'\n  └ 2026-03-20 Friday 5 CST\n\n────────────────────────────────────────────────────────────────────────────────\n\n• 能看到。\n\n  1. 今天是星期五。\n  2. 假如我是小a，那我就叫小a。\n  3. 好了。"
	if got != want {
		t.Fatalf("OutputBody() = %q, want %q", got, want)
	}
}

func TestOutputBodyKeepsNonBulletReplyStart(t *testing.T) {
	t.Parallel()

	text := "› show me a patch\n\n```diff\n+hello\n```\n\n› Implement {feature}"

	got := OutputBody(text)
	want := "```diff\n+hello\n```"
	if got != want {
		t.Fatalf("OutputBody() = %q, want %q", got, want)
	}
}

func TestIsBusyUsesTailOnly(t *testing.T) {
	t.Parallel()

	if !IsBusy("• Working (2s • esc to interrupt)") {
		t.Fatal("IsBusy() = false, want true")
	}
	if IsBusy("• Working (2s • esc to interrupt)\n\n• Final answer") {
		t.Fatal("IsBusy() = true, want false")
	}
}
